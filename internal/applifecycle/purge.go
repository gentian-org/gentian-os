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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/remotecommand"

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

func (s *Service) purge(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.AppProfile, app string) []string {
	var warnings []string
	dbEngine, s3Req, redisReq := profileKernelReqs(profile)
	if profile == nil {
		dbEngine = gentianov1alpha1.DatabaseEnginePostgreSQL
	}
	switch dbEngine {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		warnings = append(warnings, s.purgePostgres(ctx, tenant.Name, app)...)
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
	if profile == nil || profile.Spec.KernelRequirements == nil {
		return "", false, false
	}
	kr := profile.Spec.KernelRequirements
	if kr.Database != nil {
		db = kr.Database.Engine
	}
	if kr.Storage != nil && kr.Storage.S3 != nil {
		s3 = true
	}
	if kr.Cache != nil && kr.Cache.Engine == gentianov1alpha1.CacheEngineRedis {
		redis = true
	}
	return db, s3, redis
}

func sidecarNames(profile *gentianov1alpha1.AppProfile) []string {
	if profile == nil {
		return nil
	}
	out := make([]string, 0, len(profile.Spec.Sidecars))
	for _, sc := range profile.Spec.Sidecars {
		if sc.Name != "" {
			out = append(out, sc.Name)
		}
	}
	return out
}

func (s *Service) purgePostgres(ctx context.Context, tenant, app string) []string {
	dbName := dbRoleName(tenant, app)
	pod, err := s.postgresPod(ctx)
	if err != nil || pod == "" {
		return []string{fmt.Sprintf("Postgres pod not found; skipped purge for %s", dbName)}
	}
	for _, sql := range []string{
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", dbName),
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s";`, dbName),
		fmt.Sprintf(`DROP ROLE IF EXISTS "%s";`, dbName),
	} {
		if out, err := s.execPostgres(ctx, pod, sql); err != nil || strings.Contains(strings.ToUpper(out), "ERROR") {
			return []string{fmt.Sprintf("Postgres purge failed for %s: %s", dbName, out)}
		}
	}
	return nil
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
		secretEnv("MYSQL_HOST", "mariadb-admin", "host"),
		secretEnv("MYSQL_TCP_PORT", "mariadb-admin", "port"),
		secretEnv("MYSQL_PWD", "mariadb-admin", "password"),
		secretEnv("MYSQL_ADMIN_USER", "mariadb-admin", "username"),
	}
}

func minioAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("MINIO_ENDPOINT", "minio-admin", "endpoint"),
		secretEnv("MINIO_ACCESS_KEY", "minio-admin", "accessKey"),
		secretEnv("MINIO_SECRET_KEY", "minio-admin", "secretKey"),
	}
}

func redisAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("REDIS_HOST", "redis-admin", "host"),
		secretEnv("REDIS_PORT", "redis-admin", "port"),
		secretEnv("REDIS_PASSWORD", "redis-admin", "password"),
	}
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
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
	secrets, err := s.clientset.CoreV1().Secrets(s.opts.KernelNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list kernel secrets: %v", err))
	} else {
		for _, sec := range secrets.Items {
			_ = s.clientset.CoreV1().Secrets(s.opts.KernelNamespace).Delete(ctx, sec.Name, metav1.DeleteOptions{})
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

	for _, pvc := range pvcs.Items {
		shouldDelete := false

		// 1. Check if the PVC label gentianos.io/app matches the appName
		if pvc.Labels["gentianos.io/app"] == appName {
			shouldDelete = true
		}

		// 2. Check if the PVC app.kubernetes.io/instance label prefix matches appName
		if instance, ok := pvc.Labels["app.kubernetes.io/instance"]; ok {
			if strings.HasPrefix(instance, appName) {
				shouldDelete = true
			}
		}

		// 3. Check if the app.kubernetes.io/name matches appName or family
		if name, ok := pvc.Labels["app.kubernetes.io/name"]; ok {
			if name == appName || (family != "" && name == family) {
				shouldDelete = true
			}
		}

		// 4. Fallback name checks
		if strings.Contains(pvc.Name, appName) || (family != "" && strings.Contains(pvc.Name, family)) {
			shouldDelete = true
		}

		if shouldDelete {
			err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				warnings = append(warnings, fmt.Sprintf("delete PVC %s/%s: %v", ns, pvc.Name, err))
			}
		}
	}
	return warnings
}

func ptr[T any](v T) *T { return &v }
