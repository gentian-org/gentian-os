/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package applifecycle

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const mariadbDeleteScript = "" +
	"set -euo pipefail\n" +
	"if ! echo \"${DB_NAME}\" | grep -qE '^[a-zA-Z0-9_]+$'; then\n" +
	"  echo \"ERROR: invalid DB_NAME '${DB_NAME}'\" >&2; exit 1\n" +
	"fi\n" +
	"if ! echo \"${DB_USER}\" | grep -qE '^[a-zA-Z0-9_]+$'; then\n" +
	"  echo \"ERROR: invalid DB_USER '${DB_USER}'\" >&2; exit 1\n" +
	"fi\n" +
	"MARIADB=\"mariadb -h${MYSQL_HOST} -P${MYSQL_TCP_PORT} -u${MYSQL_ADMIN_USER}\"\n" +
	"$MARIADB -e \"REVOKE ALL PRIVILEGES, GRANT OPTION FROM '${DB_USER}'@'%';\" 2>/dev/null || true\n" +
	"$MARIADB -e \"DROP USER IF EXISTS '${DB_USER}'@'%';\"\n" +
	"$MARIADB -e \"DROP DATABASE IF EXISTS ${DB_NAME};\"\n" +
	"echo \"deleted database ${DB_NAME} and user ${DB_USER}\"\n"

const (
	// A purge must not return while volumes are still Terminating; NFS in
	// particular can take a while, so allow generously before declaring failure.
	pvcDeletionTimeout = 3 * time.Minute
	pvcPollInterval    = 3 * time.Second
)

func (s *Service) purge(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.AppProfile, app string) []string {
	var warnings []string
	dbEngine, s3Req, redisReq := profileKernelReqs(profile)
	if profile == nil {
		dbEngine = gentianov1alpha1.DatabaseEnginePostgreSQL
	}
	switch dbEngine {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		warnings = append(warnings, s.purgePostgres(ctx, tenant, app)...)
	case gentianov1alpha1.DatabaseEngineMariaDB:
		warnings = append(warnings, s.runMariaDBDeleteJob(ctx, tenant, app)...)
	case "":
	default:
		warnings = append(warnings, fmt.Sprintf("unsupported database engine %q for %q; skipped DB purge", dbEngine, app))
	}
	if s3Req {
		warnings = append(warnings, s.runS3DeleteJob(ctx, tenant, app)...)
	}
	if redisReq {
		warnings = append(warnings, s.runRedisDeleteJob(ctx, tenant.Name, app)...)
	}
	sidecars := sidecarNames(profile)
	warnings = append(warnings, s.purgeOpenBaoSecrets(ctx, tenant.Name, app, sidecars)...)
	warnings = append(warnings, s.purgeClusterArtifacts(ctx, tenant.Name, app)...)
	warnings = append(warnings, s.purgePVCs(ctx, tenant.Name, app, profile)...)
	return warnings
}

func profileKernelReqs(profile *gentianov1alpha1.AppProfile) (db gentianov1alpha1.DatabaseEngine, s3, redis bool) {
	stores := backup.ProfileStores(profile)
	return stores.Database, stores.S3, stores.Redis
}

func sidecarNames(profile *gentianov1alpha1.AppProfile) []string {
	return backup.SidecarNames(profile)
}

func (s *Service) purgePostgres(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) []string {
	// The database and the role are named differently -- see pgRoleName -- and
	// conflating them meant the role always survived a purge.
	dbName := databaseName(tenant, app)
	roleName := pgRoleName(tenant.Name, app)
	pod, err := s.postgresPod(ctx)
	if err != nil || pod == "" {
		return []string{fmt.Sprintf("Postgres pod not found; skipped purge for %s", dbName)}
	}
	// Apps that declare allowDynamicDatabaseCreation own every database their
	// users made, not just the provisioned one. Dropping the role would fail
	// while it still owns objects, and leaving them would strand tenant data on
	// the shared cluster under a role nobody can log in as any more. Ownership
	// is the join: CREATE DATABASE makes the creating role the owner, so this
	// finds them without the platform having to track names it never chose.
	extra, err := s.databasesOwnedBy(ctx, pod, roleName)
	if err != nil {
		return []string{fmt.Sprintf("Postgres purge could not list databases owned by %s: %v", roleName, err)}
	}

	var statements []string
	for _, db := range extra {
		statements = append(statements,
			fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", db),
			fmt.Sprintf(`DROP DATABASE IF EXISTS "%s";`, db),
		)
	}
	statements = append(statements,
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", dbName),
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s";`, dbName),
		// Anything the role still owns outside its own databases would block
		// the drop and strand the role; DROP OWNED clears those grants first.
		// Guarded because DROP OWNED errors on a missing role, and purge has to
		// stay re-runnable -- it is retried whenever an uninstall is repeated.
		fmt.Sprintf(`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') `+
			`THEN EXECUTE 'DROP OWNED BY "%s"'; END IF; END $$;`, roleName, roleName),
		fmt.Sprintf(`DROP ROLE IF EXISTS "%s";`, roleName),
	)

	for _, sql := range statements {
		if out, err := s.execPostgres(ctx, pod, sql); err != nil || strings.Contains(strings.ToUpper(out), "ERROR") {
			return []string{fmt.Sprintf("Postgres purge failed for %s: %s", dbName, out)}
		}
	}
	return nil
}

// databasesOwnedBy lists databases owned by role, excluding the app's own
// provisioned database, which the caller drops last.
func (s *Service) databasesOwnedBy(ctx context.Context, pod, role string) ([]string, error) {
	out, err := s.execPostgres(ctx, pod, fmt.Sprintf(
		"SELECT d.datname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba "+
			"WHERE r.rolname = '%s' AND d.datname <> '%s' AND NOT d.datistemplate;", role, role))
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToUpper(out), "ERROR") {
		return nil, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		// psql's default output frames the rows with a header, a rule and a
		// "(N rows)" footer; only the indented value lines are database names.
		if name == "" || strings.HasPrefix(name, "datname") || strings.HasPrefix(name, "-") ||
			strings.HasPrefix(name, "(") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (s *Service) postgresPod(ctx context.Context) (string, error) {
	pods, err := s.clientset.CoreV1().Pods(s.opts.KernelNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=postgres",
	})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}
	return pods.Items[0].Name, nil
}

func (s *Service) execPostgres(ctx context.Context, pod, sql string) (string, error) {
	req := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(s.opts.KernelNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"psql", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql},
			Stdout:    true,
			Stderr:    true,
		}, parameterCodec)
	exec, err := remoteCommandExecutor(req.URL())
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &buf,
		Stderr: &buf,
	})
	return buf.String(), err
}

func (s *Service) runKernelJob(ctx context.Context, job *batchv1.Job) []string {
	_ = s.clientset.BatchV1().Jobs(s.opts.KernelNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: ptr(metav1.DeletePropagationBackground),
	})
	if _, err := s.clientset.BatchV1().Jobs(s.opts.KernelNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return []string{fmt.Sprintf("create job %s: %v", job.Name, err)}
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		j, err := s.clientset.BatchV1().Jobs(s.opts.KernelNamespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return []string{fmt.Sprintf("get job %s: %v", job.Name, err)}
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return []string{fmt.Sprintf("job %s failed", job.Name)}
			}
		}
		select {
		case <-ctx.Done():
			return []string{ctx.Err().Error()}
		case <-time.After(3 * time.Second):
		}
	}
	return []string{fmt.Sprintf("job %s did not complete in time", job.Name)}
}

func (s *Service) runMariaDBDeleteJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) []string {
	dbName := databaseName(tenant, app)
	dbUser := mariadbUserName(tenant.Name, app)
	job := kernelDeleteJob(s.opts.KernelNamespace, mariadbDeleteJobName(tenant.Name, app), tenant.Name, app,
		kernel.MariaDBProvisionerImage(), "delete-db", mariadbDeleteScript, append(mysqlAdminEnv(),
			corev1.EnvVar{Name: "DB_NAME", Value: dbName},
			corev1.EnvVar{Name: "DB_USER", Value: dbUser},
		))
	return s.runKernelJob(ctx, job)
}

func (s *Service) runS3DeleteJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) []string {
	bucket := s3BucketName(tenant, app)
	script := fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc rb --force "gentian/%s" 2>/dev/null || echo "bucket %s already gone"
echo "bucket %s removed"`, bucket, bucket, bucket)
	job := kernelDeleteJob(s.opts.KernelNamespace, s3DeleteJobName(tenant.Name, app), tenant.Name, app,
		"minio/mc:RELEASE.2025-04-03T17-07-56Z", "delete-bucket", script, minioAdminEnv())
	return s.runKernelJob(ctx, job)
}

func (s *Service) runRedisDeleteJob(ctx context.Context, tenant, app string) []string {
	user := redisACLUsername(tenant, app)
	script := fmt.Sprintf(`set -euo pipefail
redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning \
  ACL DELUSER %s 2>/dev/null || echo "user %s already absent"
echo done`, user, user)
	job := kernelDeleteJob(s.opts.KernelNamespace, redisACLDeleteJobName(tenant, app), tenant, app,
		kernel.RedisProvisionerImage(), "del-acl-user", script, redisAdminEnv())
	return s.runKernelJob(ctx, job)
}

func kernelDeleteJob(ns, name, tenant, app, image, container, script string, env []corev1.EnvVar) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				meta.TenantLabel:    tenant,
				"gentianos.io/app":  app,
				meta.ManagedByLabel: meta.ManagedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:    container,
						Image:   image,
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{script},
						Env:     env,
					}},
				},
			},
		},
	}
}

func mysqlAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("MYSQL_HOST", "mariadb-admin", "host"),
		meta.SecretEnv("MYSQL_TCP_PORT", "mariadb-admin", "port"),
		meta.SecretEnv("MYSQL_PWD", "mariadb-admin", "password"),
		meta.SecretEnv("MYSQL_ADMIN_USER", "mariadb-admin", "username"),
	}
}

func minioAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("MINIO_ENDPOINT", "minio-admin", "endpoint"),
		meta.SecretEnv("MINIO_ACCESS_KEY", "minio-admin", "accessKey"),
		meta.SecretEnv("MINIO_SECRET_KEY", "minio-admin", "secretKey"),
	}
}

func redisAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("REDIS_HOST", "redis-admin", "host"),
		meta.SecretEnv("REDIS_PORT", "redis-admin", "port"),
		meta.SecretEnv("REDIS_PASSWORD", "redis-admin", "password"),
	}
}

func (s *Service) purgeOpenBaoSecrets(ctx context.Context, tenant, app string, sidecars []string) []string {
	keys := []string{app}
	for _, sc := range sidecars {
		keys = append(keys, app+"-"+sc)
	}
	var warnings []string
	for _, key := range keys {
		warnings = append(warnings, s.purgeOpenBaoPath(ctx, tenant, key)...)
	}
	return warnings
}

func (s *Service) purgeOpenBaoPath(ctx context.Context, tenant, appKey string) []string {
	token, err := s.operatorToken(ctx)
	if err != nil || token == "" {
		return []string{fmt.Sprintf("OpenBao purge skipped for %q (operator token unavailable)", appKey)}
	}
	pod, err := s.openbaoPod(ctx)
	if err != nil || pod == "" {
		return []string{fmt.Sprintf("OpenBao purge skipped for %q (pod unavailable)", appKey)}
	}
	base := fmt.Sprintf("gentian-os/tenants/%s/apps/%s", tenant, appKey)
	script := fmt.Sprintf(`set -eu
BAO_ADDR=http://127.0.0.1:8200
BAO_TOKEN='%s'
BASE='%s'
purge_kv_tree() {
  path="$1"
  listed=$(bao kv list -mount=secret "${path}" 2>/dev/null || true)
  if [ -z "${listed}" ]; then
    bao kv metadata delete -mount=secret "${path}" 2>/dev/null || true
    return 0
  fi
  printf '%%s\n' "${listed}" | while IFS= read -r entry; do
    [ -z "${entry}" ] && continue
    case "${entry}" in
      */) purge_kv_tree "${path}/${entry%%/}"
           ;;
      *) bao kv metadata delete -mount=secret "${path}/${entry}" 2>/dev/null || true
         ;;
    esac
  done
  bao kv metadata delete -mount=secret "${path}" 2>/dev/null || true
}
purge_kv_tree "${BASE}"
echo "OpenBao path ${BASE} purged"`, token, base)
	if out, err := s.execShell(ctx, s.opts.OpenBaoNamespace, pod, script); err != nil {
		return []string{fmt.Sprintf("OpenBao purge for %q failed: %s", appKey, out)}
	}
	return nil
}

func (s *Service) operatorToken(ctx context.Context) (string, error) {
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: ptr(int64(600)),
		},
	}
	out, err := s.clientset.CoreV1().ServiceAccounts(s.opts.OperatorNamespace).CreateToken(
		ctx, s.opts.OperatorSA, tr, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return out.Status.Token, nil
}

func (s *Service) openbaoPod(ctx context.Context) (string, error) {
	pods, err := s.clientset.CoreV1().Pods(s.opts.OpenBaoNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao",
	})
	if err == nil && len(pods.Items) > 0 {
		return pods.Items[0].Name, nil
	}
	pods, err = s.clientset.CoreV1().Pods(s.opts.OpenBaoNamespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(pods.Items) == 0 {
		return "", err
	}
	return pods.Items[0].Name, nil
}

func (s *Service) execShell(ctx context.Context, ns, pod, script string) (string, error) {
	req := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"sh", "-lc", script},
			Stdout:  true,
			Stderr:  true,
		}, parameterCodec)
	exec, err := remoteCommandExecutor(req.URL())
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &buf,
		Stderr: &buf,
	})
	return buf.String(), err
}

func (s *Service) purgeClusterArtifacts(ctx context.Context, tenant, app string) []string {
	var warnings []string
	selector := fmt.Sprintf("%s=%s,gentianos.io/app=%s,%s=%s",
		meta.TenantLabel, tenant, app, meta.ManagedByLabel, meta.ManagedByValue)
	jobs, err := s.clientset.BatchV1().Jobs(s.opts.KernelNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return []string{fmt.Sprintf("list kernel jobs: %v", err)}
	}
	for _, job := range jobs.Items {
		_ = s.clientset.BatchV1().Jobs(s.opts.KernelNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{
			PropagationPolicy: ptr(metav1.DeletePropagationBackground),
		})
	}
	// Jobs the operator creates directly in the tenant namespace (the app-admins
	// sync among them) have no owner that Crossplane deletes with the App claim,
	// so sweeping only the kernel namespace left them behind — and a finished
	// Job's pod keeps its logs and its mounted member list with it.
	tenantJobs, err := s.clientset.BatchV1().Jobs(tenantNamespace(tenant)).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list tenant jobs: %v", err))
	} else {
		for _, job := range tenantJobs.Items {
			if err := s.clientset.BatchV1().Jobs(tenantNamespace(tenant)).
				Delete(ctx, job.Name, metav1.DeleteOptions{
					PropagationPolicy: ptr(metav1.DeletePropagationBackground),
				}); err != nil && !apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf("delete tenant job %s: %v", job.Name, err))
			}
		}
	}
	// Pods outlive their Job when whatever deleted the Job did not cascade —
	// Crossplane removing the wrapping Object for a post-install Job orphans its
	// pods exactly this way, so a purged app left completed pods sitting in the
	// namespace with their logs and mounted config. Sweeping by the app selector
	// catches them whatever orphaned them; live pods of the app itself are gone
	// with the Helm release long before this runs.
	tenantPods, err := s.clientset.CoreV1().Pods(tenantNamespace(tenant)).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list tenant pods: %v", err))
	} else {
		for _, pod := range tenantPods.Items {
			if err := s.clientset.CoreV1().Pods(tenantNamespace(tenant)).
				Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf("delete tenant pod %s: %v", pod.Name, err))
			}
		}
	}
	secrets, err := s.clientset.CoreV1().Secrets(s.opts.KernelNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list kernel secrets: %v", err))
	} else {
		for _, sec := range secrets.Items {
			_ = s.clientset.CoreV1().Secrets(s.opts.KernelNamespace).Delete(ctx, sec.Name, metav1.DeleteOptions{})
		}
	}
	// The operator also writes Secrets into the tenant's own namespace — the LLM
	// credentials among them — and this only ever swept the kernel namespace, so
	// every install left one behind forever. The demo tenant had accumulated ten,
	// including some for profiles renamed out of existence months earlier.
	//
	// The selector is app-scoped, so tenant-wide Secrets such as
	// gentian-trust-anchor-tls (labelled managed-by and tenant, but no app) are not
	// matched. Secrets owned by an ExternalSecret are already removed with it when
	// Crossplane deletes the App claim.
	tenantSecrets, err := s.clientset.CoreV1().Secrets(tenantNamespace(tenant)).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list tenant secrets: %v", err))
	} else {
		for _, sec := range tenantSecrets.Items {
			if err := s.clientset.CoreV1().Secrets(tenantNamespace(tenant)).
				Delete(ctx, sec.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf("delete tenant secret %s: %v", sec.Name, err))
			}
		}
	}

	dbCR := cnpgDatabaseName(tenant, app)
	dbObj := &unstructured.Unstructured{}
	dbObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database",
	})
	dbObj.SetName(dbCR)
	dbObj.SetNamespace(s.opts.KernelNamespace)
	if err := s.client.Delete(ctx, dbObj); err != nil && !apierrors.IsNotFound(err) {
		warnings = append(warnings, fmt.Sprintf("delete CNPG database %s: %v", dbCR, err))
	}
	return warnings
}

func (s *Service) purgePVCs(ctx context.Context, tenantName, appName string, profile *gentianov1alpha1.AppProfile) []string {
	var warnings []string
	ns := tenantNamespace(tenantName)
	pvcs, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return []string{fmt.Sprintf("list tenant PVCs: %v", err)}
	}

	family := ""
	if profile != nil {
		family = profile.Spec.Family
	}

	logger := log.FromContext(ctx).WithName("purge").WithValues("app", appName)

	var doomed []string
	for _, pvc := range pvcs.Items {
		if !pvcBelongsToApp(pvc, appName, family) {
			continue
		}
		if rel, other := ownedByOtherRelease(pvc.Annotations, appName); other {
			// Deliberately not a warning: skipping is the correct outcome, and
			// warnings fail the purge.
			logger.Info("skipping PVC owned by another Helm release",
				"pvc", pvc.Name, "release", rel)
			continue
		}
		doomed = append(doomed, pvc.Name)
	}
	if len(doomed) == 0 {
		return nil
	}

	// Delete the pods still holding these volumes first.
	//
	// A PVC carries the pvc-protection finalizer while any pod references it, and
	// that includes pods which have already Succeeded — a finished install Job
	// keeps the claim alive indefinitely. Deleting the PVC alone leaves it
	// Terminating forever, and worse, a reinstall then cannot schedule ("claim is
	// being deleted") while its own Pending pods add fresh references. That is a
	// deadlock that never resolves on its own.
	warnings = append(warnings, s.releasePVCHolders(ctx, ns, doomed)...)

	for _, name := range doomed {
		err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			warnings = append(warnings, fmt.Sprintf("delete PVC %s/%s: %v", ns, name, err))
		}
	}

	// Wait for them to actually go. A purge that returns while volumes are still
	// Terminating reports success for work it has not finished, and the caller has
	// no way to know the next install will be blocked by it.
	if err := s.waitForPVCsGone(ctx, ns, doomed); err != nil {
		warnings = append(warnings, err.Error())
	}
	return warnings
}

// ownedByOtherRelease reports whether a Helm-managed object belongs to some
// release other than this app's, returning the release name for the log line.
//
// This is a veto over the matching below, and it exists because the damage is
// asymmetric. provider-helm reconciles release *state*, not cluster contents:
// it compares the desired chart and values against the recorded release and
// does nothing when they agree. Delete an object out from under a live release
// and nothing puts it back — not the next sync, not selfHeal, not a pod
// restart — until someone changes the chart version or a value. The app just
// runs without it. Meanwhile the cost of skipping wrongly is a leftover volume,
// which the next purge or an operator can remove.
//
// pvcBelongsToApp falls back to a name substring, which is broad enough to
// reach a sibling app's volume when two profiles share a family (purging
// nextcloud-base-ce matches anything containing "nextcloud"). That is precisely
// the case where deleting is unrecoverable, so Helm's own ownership record wins.
//
// Releases for one app are named after it — "<profile>-<suffix>-release" for
// the app itself, "<tenant>-<profile>-<sidecar>" for its sidecars — so a
// substring test identifies our own releases without needing the composite,
// which is already gone by the time purge runs.
func ownedByOtherRelease(annotations map[string]string, appName string) (string, bool) {
	return backup.OwnedByOtherRelease(annotations, appName)
}

// pvcBelongsToApp reports whether a PVC was provisioned for this app.
func pvcBelongsToApp(pvc corev1.PersistentVolumeClaim, appName, family string) bool {
	return backup.PVCBelongsToApp(pvc, appName, family)
}

// releasePVCHolders deletes pods referencing any of the named claims so the
// pvc-protection finalizer can clear.
func (s *Service) releasePVCHolders(ctx context.Context, ns string, claims []string) []string {
	doomed := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		doomed[c] = struct{}{}
	}

	pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []string{fmt.Sprintf("list pods holding PVCs in %s: %v", ns, err)}
	}

	var warnings []string
	for _, pod := range pods.Items {
		if !podReferencesAny(pod, doomed) {
			continue
		}
		err := s.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			warnings = append(warnings, fmt.Sprintf("delete pod %s/%s holding a purged PVC: %v", ns, pod.Name, err))
		}
	}
	return warnings
}

func podReferencesAny(pod corev1.Pod, claims map[string]struct{}) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if _, ok := claims[v.PersistentVolumeClaim.ClaimName]; ok {
			return true
		}
	}
	return false
}

// waitForPVCsGone blocks until the claims disappear, or reports which remain.
func (s *Service) waitForPVCsGone(ctx context.Context, ns string, claims []string) error {
	deadline := time.Now().Add(pvcDeletionTimeout)
	for {
		var remaining []string
		for _, name := range claims {
			_, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				remaining = append(remaining, name)
			} else if !apierrors.IsNotFound(err) {
				remaining = append(remaining, name)
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"PVCs still present after %s: %s — the next install of this app will not schedule until they are gone",
				pvcDeletionTimeout, strings.Join(remaining, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pvcPollInterval):
		}
	}
}

func ptr[T any](v T) *T { return &v }
