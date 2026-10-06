/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Purging an app: destroying what uninstalling it kept.
//
// Uninstalling takes an app's workloads and its sign-in client away and keeps
// everything it stored. A purge is the other act, asked for separately, and
// it is the one that cannot be undone. Two rules shape everything below.
//
// It is one request, answered when it is over. Nothing here continues in the
// background and nothing reconciles towards "purged": a person asked for data
// to be destroyed and is told, in the answer to that request, what was.
//
// It fails loudly. A step that could not do its work ends the purge there and
// the answer names the step, what had been destroyed before it and what was
// not attempted. Nothing is skipped because a pod, a token or a profile could
// not be found, and no command's failure is discarded. Every step can be
// repeated, so the remedy for a purge that did not complete is to ask again.

// The kinds of data an app can leave behind. The purge reports in these
// words, and so does the read of what uninstalled apps still hold
// (retained.go), so that one can be matched against the other.
const (
	KindDatabase      = "database"
	KindObjectStorage = "objectStorage"
	KindCache         = "cache"
	KindFiles         = "files"
	KindCredentials   = "credentials"
	KindAccessGroup   = "accessGroup"
	// KindRecords is what provisioning left in the cluster for the app: the
	// Jobs, their pods and the Secrets labelled with it. Not data a person
	// stored, but a finished Job's pod keeps its logs and what it mounted.
	KindRecords = "provisioningRecords"
)

// kindLabels are the kinds as a person reads them.
var kindLabels = map[string]string{
	KindDatabase:      "database",
	KindObjectStorage: "object storage",
	KindCache:         "cache",
	KindFiles:         "files",
	KindCredentials:   "stored credentials",
	KindAccessGroup:   "access group",
	KindRecords:       "provisioning records",
}

// What a purge could not look at because the app's profile is gone. See
// purgeSteps.
const (
	notExaminedMariaDB              = "database.mariadb"
	notExaminedExtensionCredentials = "credentials.extensions"
	notExaminedExtensionGroups      = "accessGroup.extensions"
	// Volume claims that carry the chart's name and nothing of the app's:
	// without the profile the chart is not known.
	notExaminedChartNamedFiles = "files.chartNamed"
)

// The bounds of one purge.
//
// purgeBudget is all the time a purge has, from the moment it holds the app's
// lock. It is the number its caller's deadline is set against: the director
// waits lifecycle.PurgeDeadline for this action, which is longer, so a purge
// is never cut off by the request that asked for it -- it either finishes or
// gives up by itself and says where. The waits below are each shorter than
// the budget and several can follow one another, so the budget is what ends a
// purge that is going badly; a wait that hits it reports that, and the retry
// starts with every finished step already done.
//
// Variables so that a test does not take minutes; nothing else assigns them.
var (
	purgeBudget = 4*time.Minute + 30*time.Second
	// purgeJobWait bounds one deletion Job, from created to finished.
	purgeJobWait = 2 * time.Minute
	// pvcDeletionTimeout bounds the wait for deleted volume claims to be
	// gone. A purge must not return while they are still Terminating: the
	// next install of the app would not schedule.
	pvcDeletionTimeout = 2 * time.Minute
	// purgeRecordWait bounds the wait for a deleted record to be gone.
	purgeRecordWait = 30 * time.Second
	purgePoll       = 3 * time.Second
)

// purgeJobDeadlineSeconds ends a deletion Job that hangs before the wait for
// it does, so the Job reports that it failed rather than the purge reporting
// only that it stopped waiting.
const purgeJobDeadlineSeconds = int64(100)

// PurgeError is a purge that did not complete.
type PurgeError struct {
	Tenant  string
	Profile string
	// Step is the kind whose destruction failed.
	Step string
	// Destroyed are the kinds destroyed before it, in order. They stay
	// destroyed: nothing is rolled back.
	Destroyed []string
	// Pending are the kinds that were not attempted.
	Pending []string
	Err     error
}

func (e *PurgeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the purge of %s in %s did not complete: destroying its %s failed: %v.",
		e.Profile, e.Tenant, kindLabels[e.Step], e.Err)
	if len(e.Destroyed) > 0 {
		fmt.Fprintf(&b, " Already destroyed: %s.", labelled(e.Destroyed))
	} else {
		b.WriteString(" Nothing had been destroyed before that.")
	}
	if len(e.Pending) > 0 {
		fmt.Fprintf(&b, " Not attempted: %s.", labelled(e.Pending))
	}
	b.WriteString(" Nothing is rolled back. Retry the purge: every step is safe to repeat, and it continues with what is left.")
	return b.String()
}

func (e *PurgeError) Unwrap() error { return e.Err }

func labelled(kinds []string) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, kindLabels[k])
	}
	return strings.Join(out, ", ")
}

// purgeStep destroys one kind of data. run returns nil only when the kind is
// gone: destroyed now, or found already absent.
type purgeStep struct {
	kind string
	run  func(ctx context.Context) error
}

// purgeSteps is what a purge of this app does, in order, and what it cannot
// look at.
//
// Which stores an app has is declared by its profile and by nothing else. An
// app whose ComponentProfile is no longer on the cluster is therefore purged
// of what every app has and of a PostgreSQL database, which is assumed; a
// bucket, a cache, a MariaDB database, the credentials of its extensions and
// the volumes that carry only its chart's name are not examined, because
// nothing says whether they exist or what they are called. Those are returned
// in notExamined and the purge's answer carries them: it reports what it
// destroyed and does not claim the rest.
func (s *Service) purgeSteps(tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile, app string) (steps []purgeStep, notExamined []string) {
	stores := backup.ProfileStores(profile)
	engine := stores.Database
	if profile == nil {
		engine = gentianov1alpha1.DatabaseEnginePostgreSQL
		notExamined = []string{KindObjectStorage, KindCache, notExaminedMariaDB,
			notExaminedExtensionCredentials, notExaminedExtensionGroups, notExaminedChartNamedFiles}
	}
	switch engine {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		steps = append(steps, purgeStep{KindDatabase, func(ctx context.Context) error {
			return s.purgePostgres(ctx, tenant, app)
		}})
	case gentianov1alpha1.DatabaseEngineMariaDB:
		steps = append(steps, purgeStep{KindDatabase, func(ctx context.Context) error {
			return s.runMariaDBDeleteJob(ctx, tenant, app)
		}})
	case "":
	default:
		steps = append(steps, purgeStep{KindDatabase, func(context.Context) error {
			return fmt.Errorf("this platform cannot drop a %q database", engine)
		}})
	}
	if stores.S3 {
		steps = append(steps, purgeStep{KindObjectStorage, func(ctx context.Context) error {
			return s.runS3DeleteJob(ctx, tenant, app)
		}})
	}
	if stores.Redis {
		steps = append(steps, purgeStep{KindCache, func(ctx context.Context) error {
			return s.runRedisDeleteJob(ctx, tenant.Name, app)
		}})
	}
	steps = append(steps,
		purgeStep{KindFiles, func(ctx context.Context) error {
			return s.purgePVCs(ctx, tenant, app, profile)
		}},
		purgeStep{KindCredentials, func(ctx context.Context) error {
			return s.purgeCredentials(ctx, tenant, app, backup.SidecarNames(profile))
		}},
		purgeStep{KindAccessGroup, func(ctx context.Context) error {
			return s.purgeAccessGroup(ctx, tenant, app, backup.SidecarNames(profile))
		}},
		// Last: the deletion Jobs above are records of this app too.
		purgeStep{KindRecords, func(ctx context.Context) error {
			return s.purgeClusterArtifacts(ctx, tenant.Name, app)
		}},
	)
	return steps, notExamined
}

// purge runs the steps in order and stops at the first that fails.
//
// Stopping is deliberate. A step that failed leaves the app in a state nobody
// has looked at, and carrying on would destroy more on the strength of it.
func (s *Service) purge(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile, app string) (destroyed, notExamined []string, err error) {
	logger := log.FromContext(ctx).WithName("purge").WithValues("tenant", tenant.Name, "app", app)
	steps, notExamined := s.purgeSteps(tenant, profile, app)
	for i, step := range steps {
		if err := step.run(ctx); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("the purge ran out of the %s it is given (%w)", purgeBudget, err)
			}
			pending := make([]string, 0, len(steps)-i-1)
			for _, later := range steps[i+1:] {
				pending = append(pending, later.kind)
			}
			logger.Error(err, "a purge step failed", "step", step.kind, "destroyed", destroyed, "pending", pending)
			return destroyed, notExamined, &PurgeError{
				Tenant: tenant.Name, Profile: app, Step: step.kind,
				Destroyed: destroyed, Pending: pending, Err: err,
			}
		}
		logger.Info("purged", "kind", step.kind)
		destroyed = append(destroyed, step.kind)
	}
	return destroyed, notExamined, nil
}

// wait pauses for one poll interval, or returns why the purge must stop.
func wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(purgePoll):
		return nil
	}
}

// --- database ---------------------------------------------------------------

func (s *Service) purgePostgres(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) error {
	// The database and the role are named differently -- see pgRoleName -- and
	// conflating them meant the role always survived a purge.
	dbName := databaseName(tenant, app)
	roleName := pgRoleName(tenant.Name, app)
	pod, err := s.postgresPrimary(ctx)
	if err != nil {
		return err
	}
	// Apps that declare allowDynamicDatabaseCreation own every database their
	// users made, not just the provisioned one. Dropping the role would fail
	// while it still owns objects, and leaving them would strand tenant data on
	// the shared cluster under a role nobody can log in as any more. Ownership
	// is the join: CREATE DATABASE makes the creating role the owner, so this
	// finds them without the platform having to track names it never chose.
	extra, err := s.databasesOwnedBy(ctx, pod, roleName)
	if err != nil {
		return fmt.Errorf("list the databases owned by %s: %w", roleName, err)
	}

	var statements []string
	for _, db := range extra {
		statements = append(statements,
			fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", db),
			fmt.Sprintf(`DROP DATABASE IF EXISTS "%s";`, db),
		)
	}
	dropOwn := []string{
		fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", dbName),
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s";`, dbName),
	}
	statements = append(statements, dropOwn...)
	statements = append(statements,
		// Anything the role still owns outside its own databases would block
		// the drop and strand the role; DROP OWNED clears those grants first.
		// Guarded because DROP OWNED errors on a missing role, and a purge has
		// to be repeatable.
		fmt.Sprintf(`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') `+
			`THEN EXECUTE 'DROP OWNED BY "%s"'; END IF; END $$;`, roleName, roleName),
		fmt.Sprintf(`DROP ROLE IF EXISTS "%s";`, roleName),
	)
	if err := s.runSQL(ctx, pod, statements); err != nil {
		return err
	}

	// The record of the database goes only now that the database has: it is
	// what says an uninstalled app still holds one, and removing it first
	// would leave a database nothing reports if the statements above failed.
	if err := s.deleteDatabaseRecord(ctx, tenant.Name, app); err != nil {
		return err
	}
	// While the record stood it asked for the database to be present, and
	// the database operator may have made it again between the drop and the
	// record going. Dropping once more closes that; it finds nothing to drop
	// otherwise.
	return s.runSQL(ctx, pod, dropOwn)
}

func (s *Service) runSQL(ctx context.Context, pod string, statements []string) error {
	for _, sql := range statements {
		out, err := s.execPostgres(ctx, pod, sql)
		if err != nil {
			return fmt.Errorf("psql on %s: %w: %s", pod, err, strings.TrimSpace(out))
		}
		if strings.Contains(strings.ToUpper(out), "ERROR") {
			return fmt.Errorf("psql on %s: %s", pod, strings.TrimSpace(out))
		}
	}
	return nil
}

// deleteDatabaseRecord removes the CloudNativePG Database object the tenant's
// provisioning made for the app, and waits for it to be gone.
func (s *Service) deleteDatabaseRecord(ctx context.Context, tenant, app string) error {
	name := cnpgDatabaseName(tenant, app)
	key := func() *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cnpgDatabaseGVK)
		obj.SetName(name)
		obj.SetNamespace(layout.System("postgresql"))
		return obj
	}
	if err := s.client.Delete(ctx, key()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the database record %s: %w", name, err)
	}
	deadline := time.Now().Add(purgeRecordWait)
	for {
		obj := key()
		err := s.client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look for the database record %s: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the database record %s is still there %s after it was deleted", name, purgeRecordWait)
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

var cnpgDatabaseGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"}

// databasesOwnedBy lists databases owned by role, excluding the app's own
// provisioned database, which the caller drops last.
func (s *Service) databasesOwnedBy(ctx context.Context, pod, role string) ([]string, error) {
	out, err := s.execPostgres(ctx, pod, fmt.Sprintf(
		"SELECT d.datname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba "+
			"WHERE r.rolname = '%s' AND d.datname <> '%s' AND NOT d.datistemplate;", role, role))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
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

// postgresPrimary is the one instance of the shared cluster that accepts
// writes.
//
// The instances carry the same cluster label, and a replica refuses DROP
// DATABASE. Taking whichever pod the API server listed first therefore
// dropped nothing whenever that was a replica -- on every cluster with more
// than one instance, some of the time. No primary is a failure, not a reason
// to skip: nothing can be dropped without one.
func (s *Service) postgresPrimary(ctx context.Context) (string, error) {
	const selector = "cnpg.io/cluster=postgres,cnpg.io/instanceRole=primary"
	ns := layout.System("postgresql")
	pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("look for the PostgreSQL primary in %s: %w", ns, err)
	}
	var running []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod.Name)
		}
	}
	switch len(running) {
	case 1:
		return running[0], nil
	case 0:
		return "", fmt.Errorf("no running PostgreSQL primary in %s (%s); nothing was dropped", ns, selector)
	default:
		return "", fmt.Errorf("%d pods in %s claim to be the PostgreSQL primary (%s); nothing was dropped",
			len(running), ns, strings.Join(running, ", "))
	}
}

func (s *Service) execPostgres(ctx context.Context, pod, sql string) (string, error) {
	return s.execInPod(ctx, layout.System("postgresql"), pod, "postgres",
		[]string{"psql", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql})
}

// execInPod runs a command in a container and returns what it printed.
func (s *Service) execInPod(ctx context.Context, ns, pod, container string, command []string) (string, error) {
	if s.exec != nil {
		return s.exec(ctx, ns, pod, container, command)
	}
	req := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
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

// --- deletion Jobs ----------------------------------------------------------

// The three scripts below run in the store's own namespace with its admin
// credential. Each ends in success only when what it was asked to remove is
// verifiably not there: no command's failure is discarded, and "already
// absent" is established by asking the store, never inferred from a failed
// delete.

// REVOKE is not issued first: it fails on a user that does not exist, which
// is why it used to be followed by `|| true`, and DROP USER takes the user's
// privileges with it.
const mariadbDeleteScript = `set -eu
if ! echo "${DB_NAME}" | grep -qE '^[a-zA-Z0-9_]+$'; then
  echo "ERROR: invalid DB_NAME '${DB_NAME}'" >&2; exit 1
fi
if ! echo "${DB_USER}" | grep -qE '^[a-zA-Z0-9_]+$'; then
  echo "ERROR: invalid DB_USER '${DB_USER}'" >&2; exit 1
fi
MARIADB="mariadb -h${MYSQL_HOST} -P${MYSQL_TCP_PORT} -u${MYSQL_ADMIN_USER}"
$MARIADB -e "DROP USER IF EXISTS '${DB_USER}'@'%';"
$MARIADB -e "DROP DATABASE IF EXISTS ${DB_NAME};"
echo "deleted database ${DB_NAME} and user ${DB_USER}"
`

// minioPurgeScript removes the bucket and the user and policy that were made
// for it. The user is found through the policy, whose statement names the
// bucket: the key pair itself was seeded and is not known to a delete Job.
//
// The same search the tenant purge's script makes, with its failures kept.
// Whether the bucket exists is read from the server's own listing, so a
// server that cannot be reached fails the Job where a failed `mc rb` followed
// by "already gone" reported success.
func minioPurgeScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" >/dev/null
buckets="$(mc ls gentian)"
if printf '%%s\n' "${buckets}" | grep -q ' %[1]s/$'; then
  mc rb --force "gentian/%[1]s"
  echo "bucket %[1]s removed"
else
  echo "bucket %[1]s is not there"
fi
policies="$(mc admin policy ls gentian)"
for policy in ${policies}; do
  case "${policy}" in *-policy) ;; *) continue ;; esac
  info="$(mc admin policy info gentian "${policy}")"
  case "${info}" in *'arn:aws:s3:::%[1]s"'*) ;; *) continue ;; esac
  user="${policy%%-policy}"
  if ! mc admin user rm gentian "${user}"; then
    if mc admin user info gentian "${user}" >/dev/null 2>&1; then
      echo "ERROR: user ${user} could not be removed" >&2; exit 1
    fi
    echo "user ${user} is not there"
  fi
  mc admin policy rm gentian "${policy}"
  echo "user ${user} and its policy removed"
done
echo "object storage for %[1]s purged"
`, bucket)
}

// redisPurgeScript removes the app's ACL user.
//
// redis-cli exits 0 when the server answers with an error, so the exit code
// says nothing. The script asks for the user list afterwards and passes only
// if the list could be read -- it always names the default user -- and the
// app's user is not in it.
//
// The user is all that is removed. The keys the app wrote stay in the shared
// instance: they carry no owner, and nothing here can tell them from another
// app's.
func redisPurgeScript(user string) string {
	return fmt.Sprintf(`set -eu
cli() { redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning "$@"; }
cli ACL DELUSER '%[1]s'
users="$(cli ACL LIST)"
case "${users}" in
  *"user default "*) ;;
  *) echo "ERROR: the cache did not list its users: ${users}" >&2; exit 1 ;;
esac
if printf '%%s\n' "${users}" | grep -q '^user %[1]s '; then
  echo "ERROR: cache user %[1]s is still there" >&2; exit 1
fi
echo "cache user %[1]s is gone"
`, user)
}

// runKernelJob runs one deletion Job to its end and reports how it ended.
func (s *Service) runKernelJob(ctx context.Context, job *batchv1.Job) error {
	jobs := s.clientset.BatchV1().Jobs(job.Namespace)
	// A Job left by an earlier purge of the same app: it has to be gone
	// before this one can be created under the same name.
	err := jobs.Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: ptr(metav1.DeletePropagationBackground),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove the previous Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	deadline := time.Now().Add(purgeJobWait)
	for {
		_, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("look for the previous Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous Job %s/%s was still there after %s", job.Namespace, job.Name, purgeJobWait)
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	deadline = time.Now().Add(purgeJobWait)
	for {
		j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return fmt.Errorf("the Job %s/%s failed (%s %s)%s", job.Namespace, job.Name,
					c.Reason, c.Message, s.jobOutput(ctx, job))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Job %s/%s had not finished after %s%s", job.Namespace, job.Name,
				purgeJobWait, s.jobOutput(ctx, job))
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

// jobOutput is the end of what a Job's last pod printed, for the message
// about its failure. Empty when it cannot be read: the failure is reported
// either way, and this only adds the reason to it.
func (s *Service) jobOutput(ctx context.Context, job *batchv1.Job) string {
	pods, err := s.clientset.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	last := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(last.CreationTimestamp.Time) {
			last = p
		}
	}
	raw, err := s.clientset.CoreV1().Pods(job.Namespace).
		GetLogs(last.Name, &corev1.PodLogOptions{TailLines: ptr(int64(10))}).DoRaw(ctx)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return "; its output ended: " + strings.Join(strings.Fields(string(raw)), " ")
}

func (s *Service) runMariaDBDeleteJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) error {
	dbName := databaseName(tenant, app)
	dbUser := mariadbUserName(tenant.Name, app)
	job := kernelDeleteJob(layout.System("mariadb"), mariadbDeleteJobName(tenant.Name, app), tenant.Name, app,
		kernel.MariaDBProvisionerImage(), "delete-db", mariadbDeleteScript, append(mysqlAdminEnv(),
			corev1.EnvVar{Name: "DB_NAME", Value: dbName},
			corev1.EnvVar{Name: "DB_USER", Value: dbUser},
		))
	return s.runKernelJob(ctx, job)
}

func (s *Service) runS3DeleteJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string) error {
	job := kernelDeleteJob(layout.System("s3"), s3DeleteJobName(tenant.Name, app), tenant.Name, app,
		"quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z", "delete-bucket",
		minioPurgeScript(s3BucketName(tenant, app)), minioAdminEnv())
	return s.runKernelJob(ctx, job)
}

func (s *Service) runRedisDeleteJob(ctx context.Context, tenant, app string) error {
	job := kernelDeleteJob(layout.System("cache"), redisACLDeleteJobName(tenant, app), tenant, app,
		kernel.RedisProvisionerImage(), "del-acl-user",
		redisPurgeScript(redisACLUsername(tenant, app)), redisAdminEnv())
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
			// One retry and a deadline, so a Job that cannot succeed says so
			// while the purge is still waiting for it.
			BackoffLimit:          ptr(int32(1)),
			ActiveDeadlineSeconds: ptr(purgeJobDeadlineSeconds),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
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

// --- stored credentials -----------------------------------------------------

// CredentialStore is the vault as a purge and the retained-data read use it:
// remove a subtree, and name what is below a path. *secrets.KVClient is one.
type CredentialStore interface {
	DeleteTree(ctx context.Context, logicalPath string) error
	ListChildren(ctx context.Context, logicalPath string) ([]string, error)
}

// ownedKeys are the names an app's stores are kept under: its own, and one
// per extension, which the app Composition keys "{app}-{extension}". An
// extension's vault path, its access group and its Helm release are all named
// from its key, exactly as the app's own are from the app's name.
//
// A key that is itself an app or add-on of the tenant is left out. Nothing
// stops an app being called what another app's extension key spells, and what
// an installed app holds is not another app's to destroy.
func ownedKeys(tenant *gentianov1alpha1.Tenant, app string, extensions []string) []string {
	inUse := tenantApps(tenant)
	keys := []string{app}
	for _, ext := range extensions {
		if key := app + "-" + ext; !inUse[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// tenantApps are the apps and add-ons the tenant has.
func tenantApps(tenant *gentianov1alpha1.Tenant) map[string]bool {
	out := map[string]bool{}
	for _, a := range tenant.Spec.Apps {
		out[a.Profile] = true
		for _, addon := range a.Addons {
			out[addon] = true
		}
	}
	return out
}

// credentialPaths are the vault subtrees one app owns: its own, and one per
// extension. Everything provisioning seeds for the app (its database, bucket,
// cache, mail and sign-in credentials) and every secret generated for it is
// below one of these.
//
// A contract's path (…/contracts/<name>) is not among them: it is shared by
// the app that offers it and every app that consumes it, and belongs to none.
func credentialPaths(tenant *gentianov1alpha1.Tenant, app string, extensions []string) []string {
	var paths []string
	for _, key := range ownedKeys(tenant, app, extensions) {
		paths = append(paths, secrets.AppPath(tenant.Name, key))
	}
	return paths
}

// purgeCredentials deletes every vault path the app owns, all versions and
// their metadata, over the operator's own authenticated session.
//
// This used to exec a script into the vault's pod. The script addressed the
// vault at a name it does not answer on, and every command in it ended in
// `|| true`: it deleted nothing and the purge reported the credentials
// destroyed. A path that cannot be deleted fails the purge
// now, and so does a cluster where the operator has no vault to talk to.
func (s *Service) purgeCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string, extensions []string) error {
	if s.vault == nil {
		return errors.New("the operator has no connection to the vault (BAO_ADDR is not set); no credential was deleted")
	}
	for _, path := range credentialPaths(tenant, app, extensions) {
		if err := s.vault.DeleteTree(ctx, path); err != nil {
			return fmt.Errorf("delete the vault path %s: %w", path, err)
		}
	}
	return nil
}

// --- access group -----------------------------------------------------------

// AccessGroups is the identity provider as a purge and the retained-data read
// use it. *authz.KeycloakAdminClient is one.
type AccessGroups interface {
	DeleteGroup(ctx context.Context, realm, groupName string) (existed bool, err error)
	GroupNames(ctx context.Context, realm, prefix string) ([]string, error)
}

// purgeAccessGroup removes the app's group, and with it everybody's
// membership of it; and the same for each of its extensions, which has a
// group of its own when it signs people in.
//
// The group is what says who may use the app. Uninstalling keeps it, so that
// an app installed again is open to the people it was open to; a purge is the
// end of the app in this tenant, and a group left behind would hand a later
// app of the same name to whoever had the old one.
//
// The realm is the tenant's name, as it is wherever this API talks to the
// identity provider.
func (s *Service) purgeAccessGroup(ctx context.Context, tenant *gentianov1alpha1.Tenant, app string, extensions []string) error {
	groups, err := s.accessGroups(ctx)
	if err != nil {
		return err
	}
	for _, key := range ownedKeys(tenant, app, extensions) {
		name := keycloak.TenantAppGroup(tenant.Name, key)
		if _, err := groups.DeleteGroup(ctx, tenant.Name, name); err != nil {
			return fmt.Errorf("delete the group %s in realm %s: %w", name, tenant.Name, err)
		}
	}
	return nil
}

// --- provisioning records ---------------------------------------------------

func (s *Service) purgeClusterArtifacts(ctx context.Context, tenant, app string) error {
	selector := fmt.Sprintf("%s=%s,gentianos.io/app=%s,%s=%s",
		meta.TenantLabel, tenant, app, meta.ManagedByLabel, meta.ManagedByValue)
	background := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}

	// Jobs: the kernel's, and the ones the operator creates directly in the
	// tenant namespace (the app-admins sync among them), which have no owner
	// that Crossplane deletes with the App claim.
	for _, ns := range append(platformNamespaces(), tenantNamespace(tenant)) {
		jobs, err := s.clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list the app's Jobs in %s: %w", ns, err)
		}
		for _, job := range jobs.Items {
			if err := s.clientset.BatchV1().Jobs(ns).Delete(ctx, job.Name, background); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete Job %s/%s: %w", ns, job.Name, err)
			}
		}
	}
	// Pods outlive their Job when whatever deleted the Job did not cascade —
	// Crossplane removing the wrapping Object for a post-install Job orphans its
	// pods exactly this way, so a purged app left completed pods sitting in the
	// namespace with their logs and mounted config. Sweeping by the app selector
	// catches them whatever orphaned them; live pods of the app itself are gone
	// with the Helm release before a purge is admitted.
	tenantPods, err := s.clientset.CoreV1().Pods(tenantNamespace(tenant)).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list the app's pods: %w", err)
	}
	for _, pod := range tenantPods.Items {
		if err := s.clientset.CoreV1().Pods(tenantNamespace(tenant)).
			Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
	}
	// Secrets: the kernel's, and the ones the operator writes into the
	// tenant's own namespace — the LLM credentials among them.
	//
	// The selector is app-scoped, so tenant-wide Secrets such as
	// gentian-trust-anchor-tls (labelled managed-by and tenant, but no app) are not
	// matched. Secrets owned by an ExternalSecret are already removed with it when
	// Crossplane deletes the App claim.
	for _, ns := range append(platformNamespaces(), tenantNamespace(tenant)) {
		list, err := s.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list the app's Secrets in %s: %w", ns, err)
		}
		for _, sec := range list.Items {
			if err := s.clientset.CoreV1().Secrets(ns).Delete(ctx, sec.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete Secret %s/%s: %w", ns, sec.Name, err)
			}
		}
	}
	return nil
}

// --- files ------------------------------------------------------------------

// appVolumes are the claims in the tenant's namespace that are this app's:
// what a purge deletes, and what the retained-data read reports. One rule for
// both, so that what is reported as kept is what a purge would destroy.
//
// A claim is the app's when it matches by label or by name (pvcBelongsToApp)
// and is not on record as another release's. The second half is a veto over
// the first, and it exists because the damage is asymmetric. The match falls
// back to a name substring, which reaches a sibling's volume when two
// profiles share a chart (purging nextcloud-base-ce matches anything
// containing "nextcloud") or when one app's name begins another's.
// provider-helm reconciles release *state*, not cluster contents: delete an
// object out from under a live release and nothing puts it back — not the
// next sync, not selfHeal, not a pod restart — until someone changes the
// chart version or a value. The app just runs without it. The cost of leaving
// a claim wrongly is a leftover volume, which is reported in vetoed.
//
// So a claim that names a release is the app's only if the release is: the
// app's own, one of its declared extensions', or the one a chart delivered
// without the app Composition is installed as. Those names are exact
// (backup.AppRelease and its neighbours), which is also how a claim an
// uninstall kept is known to be this app's: it still carries its release.
// When the profile is gone the extensions are not known, and any release
// shaped like one of the app's counts — unless it is the release of an app
// the tenant has. A claim that names no release is matched by label and name
// alone, as before.
//
// The chart's name is what a claim's app.kubernetes.io/name is likely to be
// when it is not the install's own name. Without a profile it is empty, which
// only narrows the match: a volume is left behind rather than a sibling's
// taken with it, which is the right way round for a purge.
func appVolumes(claims []corev1.PersistentVolumeClaim, tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile) (own []string, vetoed map[string]string) {
	vetoed = map[string]string{}
	chart := chartName(profile)
	for _, pvc := range claims {
		if !pvcBelongsToApp(pvc, app, chart) {
			continue
		}
		if release := claimRelease(pvc); release != "" && !ownsRelease(tenant, app, profile, release) {
			vetoed[pvc.Name] = release
			continue
		}
		own = append(own, pvc.Name)
	}
	return own, vetoed
}

// claimRelease is the Helm release a claim is on record as belonging to: the
// one Helm annotated it with, for a claim the chart templated; the one its
// instance label names, for a claim a StatefulSet of the chart made.
func claimRelease(pvc corev1.PersistentVolumeClaim) string {
	if release := pvc.Annotations["meta.helm.sh/release-name"]; release != "" {
		return release
	}
	return pvc.Labels["app.kubernetes.io/instance"]
}

// ownsRelease reports whether a Helm release in the tenant's namespace is
// this app's and nobody else's.
func ownsRelease(tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile, release string) bool {
	// The release of an app or add-on the tenant has is that app's, whatever
	// else its name resembles.
	if key, ok := strings.CutSuffix(release, "-release"); ok && key != app && tenantApps(tenant)[key] {
		return false
	}
	extensions := make([]string, 0)
	for _, key := range ownedKeys(tenant, app, backup.SidecarNames(profile))[1:] {
		extensions = append(extensions, strings.TrimPrefix(key, app+"-"))
	}
	return backup.IsAppRelease(release, tenant.Name, app, extensions, profile != nil)
}

func chartName(profile *gentianov1alpha1.ComponentProfile) string {
	if chart := profile.Chart(); chart != nil {
		return chart.Name
	}
	return ""
}

func (s *Service) purgePVCs(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.ComponentProfile) error {
	ns := tenantNamespace(tenant.Name)
	pvcs, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the volume claims in %s: %w", ns, err)
	}
	doomed, vetoed := appVolumes(pvcs.Items, tenant, appName, profile)
	logger := log.FromContext(ctx).WithName("purge").WithValues("app", appName)
	for name, rel := range vetoed {
		// Not a failure: leaving it is the correct outcome.
		logger.Info("leaving a volume claim that is on record as another Helm release's", "pvc", name, "release", rel)
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
	if err := s.releasePVCHolders(ctx, ns, doomed); err != nil {
		return err
	}
	for _, name := range doomed {
		err := s.clientset.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete volume claim %s/%s: %w", ns, name, err)
		}
	}

	// Wait for them to actually go. A purge that returns while volumes are still
	// Terminating reports success for work it has not finished, and the caller has
	// no way to know the next install will be blocked by it.
	return s.waitForPVCsGone(ctx, ns, doomed)
}

// pvcBelongsToApp reports whether a PVC was provisioned for this app.
func pvcBelongsToApp(pvc corev1.PersistentVolumeClaim, appName, family string) bool {
	return backup.PVCBelongsToApp(pvc, appName, family)
}

// releasePVCHolders deletes pods referencing any of the named claims so the
// pvc-protection finalizer can clear.
func (s *Service) releasePVCHolders(ctx context.Context, ns string, claims []string) error {
	doomed := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		doomed[c] = struct{}{}
	}

	pods, err := s.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the pods holding volume claims in %s: %w", ns, err)
	}
	for _, pod := range pods.Items {
		if !podReferencesAny(pod, doomed) {
			continue
		}
		err := s.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s/%s, which holds a volume claim being purged: %w", ns, pod.Name, err)
		}
	}
	return nil
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
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("look for volume claim %s/%s: %w", ns, name, err)
			}
			remaining = append(remaining, name)
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"volume claims still present %s after they were deleted: %s — the next install of this app will not schedule until they are gone",
				pvcDeletionTimeout, strings.Join(remaining, ", "))
		}
		if err := wait(ctx); err != nil {
			return fmt.Errorf("waiting for volume claims %s to be gone: %w", strings.Join(remaining, ", "), err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// platformNamespaces are where tenant provisioning leaves objects: one per
// function, as internal/controller addresses them.
func platformNamespaces() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, ns := range []string{
		layout.Namespace(layout.Authentication),
		layout.Namespace(layout.Provisioning),
		layout.System("postgresql"),
		layout.System("mariadb"),
		layout.System("s3"),
		layout.System("cache"),
		layout.System("mail"),
	} {
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}
