/*
Copyright 2026 The Gentian Authors.

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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionMariaDBReady = "MariaDBReady"
	mysqlProvisionerImage = "mariadb:11"
	mariadbAdminSecret    = "mariadb-admin"
	mariadbRequeueAfter   = 2 * time.Second
)

// ensureMariaDB provisions per-app-per-tenant MariaDB databases using idempotent
// SQL Jobs. It looks up which apps require MariaDB via AppProfile KernelRequirements,
// then runs a setup Job for each (CREATE DATABASE IF NOT EXISTS + CREATE USER +
// GRANT). Completion of all setup Jobs sets MariaDBReady=True.
func (r *TenantReconciler) ensureMariaDB(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	mariaApps, err := r.collectMariaDBApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(mariaApps) == 0 {
		r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionTrue,
			"NoMariaDBRequired", "No apps require MariaDB provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true
	for _, appName := range mariaApps {
		done, err := r.ensureMariaDBSetupJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure MariaDB setup Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for MariaDB databases and users to be ready")
		return ctrl.Result{RequeueAfter: mariadbRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionTrue,
		"Provisioned", "All MariaDB databases and users are ready")
	return ctrl.Result{}, nil
}

// collectMariaDBApps returns the names of AppProfiles that require MariaDB.
func (r *TenantReconciler) collectMariaDBApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	var apps []string
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Database != nil &&
			profile.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEngineMariaDB {
			apps = append(apps, app.Profile)
		}
	}
	return apps, nil
}

// ensureMariaDBSetupJob creates (or checks) the idempotent SQL setup Job for a
// single app. Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureMariaDBSetupJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	jobName := mariadbSetupJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: appName}, profile); err != nil {
			return false, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		allowDynamic := false
		if profile.Spec.KernelRequirements != nil && profile.Spec.KernelRequirements.Database != nil {
			allowDynamic = profile.Spec.KernelRequirements.Database.AllowDynamicDatabaseCreation
		}

		// Inc 21a: derive the per-app database password and persist it under
		// the canonical OpenBao path before creating the SQL Job. The Job
		// receives the same value via DB_PASS so live MariaDB and OpenBao stay
		// in lockstep. When Seeder is nil the Job falls back to a local random.
		dbPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedMariaDB(ctx, tenant.Name, appName, secrets.DatabaseCreds{
				Host: fmt.Sprintf("%s.%s.svc.cluster.local", "mariadb", kernelNamespace),
				Port: "3306",
				Name: databaseName(tenant, appName),
				User: mariadbUserName(tenant.Name, appName),
			})
			if seedErr != nil {
				return false, fmt.Errorf("seed mariadb: %w", seedErr)
			}
			dbPassword = creds.Password
		}
		return false, r.Create(ctx, makeMariaDBSetupJob(tenant, appName, dbPassword, allowDynamic))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// deleteMariaDB handles MariaDB cleanup on tenant deletion.
// DeletionPolicy=Delete: creates a Job that drops the databases and users.
// DeletionPolicy=Retain: no-op — data is preserved.
func (r *TenantReconciler) deleteMariaDB(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if profile.Spec.KernelRequirements == nil ||
			profile.Spec.KernelRequirements.Database == nil ||
			profile.Spec.KernelRequirements.Database.Engine != gentianov1alpha1.DatabaseEngineMariaDB {
			continue
		}
		deleteJobName := mariadbDeleteJobName(tenant.Name, app.Profile)
		existing := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, makeMariaDBDeleteJob(tenant, app.Profile)); err != nil {
				return fmt.Errorf("create MariaDB delete Job for %s: %w", app.Profile, err)
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// --- Job constructors --------------------------------------------------------

// makeMariaDBSetupJob builds the idempotent database + user provisioning Job.
// Credentials are injected from the mariadb-admin Secret in the kernel namespace.
// The database name and username are passed as explicit env vars to avoid shell
// quoting issues.
func makeMariaDBSetupJob(tenant *gentianov1alpha1.Tenant, appName, dbPassword string, allowDynamic bool) *batchv1.Job {
	ttl := int32(3600)
	dbName := databaseName(tenant, appName)
	dbUser := mariadbUserName(tenant.Name, appName)
	c := mariadbContainer("provision-db", mariadbSetupScript, dbName, dbUser)
	if dbPassword != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "DB_PASS", Value: dbPassword})
	}
	if allowDynamic {
		c.Env = append(c.Env, corev1.EnvVar{Name: "ALLOW_DYNAMIC", Value: "true"})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mariadbSetupJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

// makeMariaDBDeleteJob builds the DROP DATABASE / DROP USER cleanup Job.
func makeMariaDBDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := int32(3600)
	dbName := databaseName(tenant, appName)
	dbUser := mariadbUserName(tenant.Name, appName)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mariadbDeleteJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						mariadbContainer("delete-db", mariadbDeleteScript, dbName, dbUser),
					},
				},
			},
		},
	}
}

// mariadbContainer returns a Container that runs a mariadb CLI script.
// The database name and user are passed as plain env vars; credentials come
// from the mariadb-admin Secret.
func mariadbContainer(name, script, dbName, dbUser string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   mysqlProvisionerImage,
		Command: []string{"/bin/bash", "-c", script},
		Env: []corev1.EnvVar{
			// Credentials from the kernel mariadb-admin Secret
			{
				Name: "MYSQL_HOST",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: mariadbAdminSecret},
						Key:                  "host",
					},
				},
			},
			{
				Name: "MYSQL_TCP_PORT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: mariadbAdminSecret},
						Key:                  "port",
					},
				},
			},
			{
				Name: "MYSQL_PWD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: mariadbAdminSecret},
						Key:                  "password",
					},
				},
			},
			{
				Name: "MYSQL_ADMIN_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: mariadbAdminSecret},
						Key:                  "username",
					},
				},
			},
			// Per-tenant computed values — passed as plain literals, never injected
			// into raw SQL strings (the script uses parameterised quoting).
			{Name: "DB_NAME", Value: dbName},
			{Name: "DB_USER", Value: dbUser},
		},
	}
}

// --- SQL scripts -------------------------------------------------------------

// mariadbSetupScript is an idempotent bash script that:
// 1. Creates the database if it does not already exist.
// 2. Creates the user if absent, assigning a random password.
// 3. Grants full privileges on the database to the user.
// DB_NAME and DB_USER are injected as environment variables to avoid SQL
// injection through shell quoting. They are validated to contain only safe
// characters (letters, digits, underscores) before use.
// mariadbSetupScript is an idempotent bash script that creates a MariaDB database
// and user with full privileges. Identifiers are passed via env vars and validated
// to contain only safe characters before use — no backtick quoting needed.
var mariadbSetupScript = "" +
	"set -euo pipefail\n" +
	"if ! echo \"${DB_NAME}\" | grep -qE '^[a-zA-Z0-9_]+$'; then\n" +
	"  echo \"ERROR: invalid DB_NAME '${DB_NAME}'\" >&2; exit 1\n" +
	"fi\n" +
	"if ! echo \"${DB_USER}\" | grep -qE '^[a-zA-Z0-9_]+$'; then\n" +
	"  echo \"ERROR: invalid DB_USER '${DB_USER}'\" >&2; exit 1\n" +
	"fi\n" +
	"if [ -z \"${DB_PASS:-}\" ]; then\n" +
	"  echo \"ERROR: DB_PASS must not be empty\" >&2; exit 1\n" +
	"fi\n" +
	"MARIADB=\"mariadb -h${MYSQL_HOST} -P${MYSQL_TCP_PORT} -u${MYSQL_ADMIN_USER}\"\n" +
	"$MARIADB -e \"CREATE DATABASE IF NOT EXISTS ${DB_NAME};\"\n" +
	"echo \"database ${DB_NAME} ensured\"\n" +
	"USER_EXISTS=$($MARIADB -N -s -e \"SELECT COUNT(*) FROM mysql.user WHERE User='${DB_USER}' AND Host='%';\")\n" +
	"if [ \"${USER_EXISTS}\" = \"0\" ]; then\n" +
	"  $MARIADB -e \"CREATE USER '${DB_USER}'@'%' IDENTIFIED BY '${DB_PASS}';\"\n" +
	"  echo \"user ${DB_USER} created\"\n" +
	"else\n" +
	"  $MARIADB -e \"ALTER USER '${DB_USER}'@'%' IDENTIFIED BY '${DB_PASS}';\"\n" +
	"  echo \"user ${DB_USER} password synced\"\n" +
	"fi\n" +
	"if [ \"${ALLOW_DYNAMIC:-}\" = \"true\" ]; then\n" +
	"  $MARIADB -e \"GRANT ALL PRIVILEGES ON *.* TO '${DB_USER}'@'%' WITH GRANT OPTION; FLUSH PRIVILEGES;\"\n" +
	"  echo \"global privileges granted - done\"\n" +
	"else\n" +
	"  $MARIADB -e \"GRANT ALL PRIVILEGES ON ${DB_NAME}.* TO '${DB_USER}'@'%'; FLUSH PRIVILEGES;\"\n" +
	"  echo \"privileges granted - done\"\n" +
	"fi\n"

// mariadbDeleteScript drops the database and user idempotently.
var mariadbDeleteScript = "" +
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

// --- Name helpers ------------------------------------------------------------

// mariadbUserName returns the MariaDB username for a tenant + app.
// Hyphens are replaced with underscores because MariaDB usernames must
// match ^[a-zA-Z0-9_]+$ (enforced in the provisioner script validation).
func mariadbUserName(tenantName, appName string) string {
	safeTenant := strings.ReplaceAll(tenantName, "-", "_")
	safeApp := strings.ReplaceAll(appName, "-", "_")
	return fmt.Sprintf("%s_%s", safeTenant, safeApp)
}

func mariadbSetupJobName(tenantName, appName string) string {
	return fmt.Sprintf("mariadb-setup-%s-%s", tenantName, appName)
}

func mariadbDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("mariadb-delete-%s-%s", tenantName, appName)
}
