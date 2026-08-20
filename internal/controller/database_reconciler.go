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

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionDatabaseReady = "DatabaseReady"
	cnpgGroup              = "postgresql.cnpg.io"
	cnpgVersion            = "v1"
	cnpgDatabaseKind       = "Database"
	postgresAdminSecret    = "postgres-admin"
	databaseRequeueAfter   = 2 * time.Second
)

// cnpgClusterName is the shared CloudNativePG Cluster in platform-kernel.
var cnpgClusterName = envOrDefault("CNPG_CLUSTER_NAME", "postgres")

// ensureDatabase provisions per-app-per-tenant PostgreSQL databases via
// CloudNativePG Database CRs and per-app role Jobs.
func (r *TenantReconciler) ensureDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	portalShellDone, err := r.ensurePortalShellDatabase(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure portal shell database: %w", err)
	}
	if !portalShellDone {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse,
			"ProvisioningPortalShell", "Waiting for portal shell PostgreSQL database")
		return r.requeueForPendingJob(ctx, tenant.Name, roleJobName(tenant.Name, portalShellAppName)), nil
	}

	pgApps, err := r.collectPostgresApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(pgApps) == 0 {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionTrue,
			"PortalShellReady", "Portal shell database ready; no app databases required")
		return ctrl.Result{}, nil
	}

	nsName := tenantNamespaceName(tenant)
	allDone := true
	var pendingJobs []string

	for _, appName := range pgApps {
		dbName := databaseName(tenant, appName)

		roleJobDone, err := r.ensureRoleJob(ctx, tenant, nsName, dbName, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure role Job for app %s: %w", appName, err)
		}
		if !roleJobDone {
			allDone = false
			pendingJobs = append(pendingJobs, roleJobName(tenant.Name, appName))
			continue
		}

		dbReady, err := r.ensureDatabaseCR(ctx, tenant, nsName, dbName, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Database CR for app %s: %w", appName, err)
		}
		if !dbReady {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for PostgreSQL databases and roles to be ready")
		return r.requeueForPendingJob(ctx, tenant.Name, pendingJobs...), nil
	}

	r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionTrue,
		"Provisioned", "All PostgreSQL databases and roles are ready")
	return ctrl.Result{}, nil
}

// collectPostgresApps returns profile names of apps that require a per-tenant
// PostgreSQL database.
func (r *TenantReconciler) collectPostgresApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	var pgApps []string
	for _, app := range tenant.Spec.Apps {
		profile, ok := appProfileFromIndex(profileIndex, app.Profile)
		if !ok {
			continue
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Database != nil &&
			profile.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEnginePostgreSQL {
			pgApps = append(pgApps, app.Profile)
		}
	}
	return pgApps, nil
}

// schemaPreferenceFor reports the search_path order an app's profile asked for.
// Unset means the app's own schema comes first, which is the safe default: it is
// what an app doing its own schema isolation expects, and it is the behaviour
// every app had before the preference was declarable.
func schemaPreferenceFor(profile *gentianov1alpha1.AppProfile) gentianov1alpha1.SchemaPreference {
	if profile == nil || profile.Spec.KernelRequirements == nil ||
		profile.Spec.KernelRequirements.Database == nil {
		return gentianov1alpha1.SchemaPreferenceAppSchema
	}
	if pref := profile.Spec.KernelRequirements.Database.SchemaPreference; pref != "" {
		return pref
	}
	return gentianov1alpha1.SchemaPreferenceAppSchema
}

// allowsDynamicDatabaseCreation reports whether the app may create databases of
// its own at runtime. Only apps whose whole purpose is letting a user make
// databases should ask for it — every database created this way is owned by the
// app role, which is what lets the purge find and drop them again.
func allowsDynamicDatabaseCreation(profile *gentianov1alpha1.AppProfile) bool {
	if profile == nil || profile.Spec.KernelRequirements == nil ||
		profile.Spec.KernelRequirements.Database == nil {
		return false
	}
	return profile.Spec.KernelRequirements.Database.AllowDynamicDatabaseCreation
}

// ensureDatabaseCR waits for the Crossplane-owned CloudNativePG Database CR.
func (r *TenantReconciler) ensureDatabaseCR(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) (bool, error) {
	crName := databaseCRName(tenant.Name, appName)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})
	err := r.Get(ctx, types.NamespacedName{Name: crName, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cnpgDatabaseIsReady(existing), nil
}

// ensureRoleJob waits for the Crossplane-owned psql role Job.
func (r *TenantReconciler) ensureRoleJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, roleJobName(tenant.Name, appName))
}

// deleteDatabase handles database cleanup on tenant deletion.
// DeletionPolicy=Delete: deletes all tenant-labeled CloudNativePG Database CRs
// (including databases from previously uninstalled apps).
// DeletionPolicy=Retain: no-op — databases and data are preserved.
func (r *TenantReconciler) deleteDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	return r.deleteTenantLabeledDatabaseCRs(ctx, tenant.Name)
}

// --- CR constructors ---------------------------------------------------------

func buildDatabaseCR(tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) *unstructured.Unstructured {
	roleName := roleUserName(tenant.Name, appName)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})
	obj.SetName(databaseCRName(tenant.Name, appName))
	obj.SetNamespace(kernelNamespace) // must be in the same namespace as the CNPG Cluster
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
		appLabel:       appName,
	})
	// CloudNativePG Database CR spec: references the shared cluster and declares
	// the database name and owner role.
	if err := unstructured.SetNestedField(obj.Object, cnpgClusterName, "spec", "cluster", "name"); err == nil {
		_ = unstructured.SetNestedField(obj.Object, dbName, "spec", "name")
		_ = unstructured.SetNestedField(obj.Object, roleName, "spec", "owner")
		_ = unstructured.SetNestedField(obj.Object, "present", "spec", "ensure")
	}
	return obj
}

// makeRoleJob creates a psql Job that creates the per-app PostgreSQL role
// and grants full privileges on the provisioned database.
//
// When rolePassword is non-empty (Inc 21a — Seeder enabled) the Job applies
// that exact password to the role via CREATE/ALTER ROLE, so the live
// PostgreSQL password equals the OpenBao-seeded value. When empty, the Job
// generates a random password locally (legacy behaviour).
func makeRoleJob(tenant *gentianov1alpha1.Tenant, nsName, dbName, appName, rolePassword string, schemaPref gentianov1alpha1.SchemaPreference, allowCreateDB bool) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	roleName := roleUserName(tenant.Name, appName)
	container := psqlContainer("provision-role", buildRoleScript(dbName, roleName, schemaPref, allowCreateDB), nsName)
	if rolePassword != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "ROLE_PW",
			Value: rolePassword,
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleJobName(tenant.Name, appName),
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
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

// psqlContainer returns a Container that runs a psql script via the postgres provisioner image.
// Credentials are injected from the postgres-admin Secret in the kernel namespace.
func psqlContainer(name, script, tenantNamespace string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/bash", "-c", script},
		Env: []corev1.EnvVar{
			{
				Name: "PGHOST",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: postgresAdminSecret},
						Key:                  "host",
					},
				},
			},
			{
				Name: "PGPORT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: postgresAdminSecret},
						Key:                  "port",
					},
				},
			},
			{
				Name: "PGUSER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: postgresAdminSecret},
						Key:                  "username",
					},
				},
			},
			{
				Name: "PGPASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: postgresAdminSecret},
						Key:                  "password",
					},
				},
			},
		},
	}
}

// --- Shell scripts -----------------------------------------------------------

func buildRoleScript(dbName, roleName string, schemaPref gentianov1alpha1.SchemaPreference, allowCreateDB bool) string {
	// When ROLE_PW is provided (Inc 21a — Seeder enabled) the script applies
	// that exact password to the role via CREATE/ALTER ROLE WITH PASSWORD,
	// keeping the live PostgreSQL password in lockstep with OpenBao. When
	// unset, a random password is generated locally and discarded after Job
	// completion (legacy behaviour — apps without seeded OpenBao cannot
	// connect, but backends that only need the database to exist still work).
	// The app's own schema first, unless the profile asked for public first
	// (spec.kernelRequirements.database.schemaPreference).
	searchPath := fmt.Sprintf("\\\"%s\\\", public", dbName)
	if schemaPref == gentianov1alpha1.SchemaPreferencePublic {
		searchPath = fmt.Sprintf("public, \\\"%s\\\"", dbName)
	}

	script := fmt.Sprintf(`set -euo pipefail
if [ -z "${ROLE_PW:-}" ]; then
  ROLE_PW=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)
fi
ROLE_EXISTS=$(psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='%s'" postgres)
if [ "${ROLE_EXISTS}" != "1" ]; then
  psql -c "CREATE ROLE \"%s\" WITH LOGIN PASSWORD '${ROLE_PW}';" postgres
  echo "role %s created"
else
  psql -c "ALTER ROLE \"%s\" WITH LOGIN PASSWORD '${ROLE_PW}';" postgres
  echo "role %s password updated"
fi
DB_EXISTS=$(psql -tAc "SELECT 1 FROM pg_database WHERE datname='%s'" postgres)
if [ "${DB_EXISTS}" != "1" ]; then
  psql -c "CREATE DATABASE \"%s\" OWNER \"%s\";" postgres
  echo "database %s created"
fi
psql -c "GRANT ALL PRIVILEGES ON DATABASE \"%s\" TO \"%s\";" postgres
# Some catalogue apps (e.g. XWiki) use PostgreSQL schema virtual_mode: tables live in a schema
# matching the database name (e.g. demo_xwiki), not public.
psql -d "%s" -v ON_ERROR_STOP=1 -c "CREATE SCHEMA IF NOT EXISTS \"%s\" AUTHORIZATION \"%s\";"
psql -d "%s" -v ON_ERROR_STOP=1 -c "GRANT ALL ON SCHEMA \"%s\" TO \"%s\";"
psql -c "ALTER ROLE \"%s\" SET search_path TO %s;" postgres
echo "schema %s ensured"
echo "privileges granted"`, roleName, roleName, roleName, roleName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, roleName, searchPath, dbName)

	// Stated both ways rather than only when enabled: a profile that turns the
	// permission back off must actually lose it, and this Job is the only thing
	// that ever reconciles the role.
	if allowCreateDB {
		script += fmt.Sprintf("\npsql -c \"ALTER ROLE \\\"%s\\\" WITH CREATEDB;\" postgres\necho \"createdb granted\"", roleName)
	} else {
		script += fmt.Sprintf("\npsql -c \"ALTER ROLE \\\"%s\\\" WITH NOCREATEDB;\" postgres\necho \"createdb revoked\"", roleName)
	}
	return script
}

// --- Status helpers ----------------------------------------------------------

// cnpgDatabaseIsReady returns true when a CloudNativePG Database CR has been
// applied successfully (status.applied == true). CNPG sets this field after
// the database has been created in PostgreSQL.
func cnpgDatabaseIsReady(obj *unstructured.Unstructured) bool {
	applied, found, err := unstructured.NestedBool(obj.Object, "status", "applied")
	if err != nil || !found {
		return false
	}
	return applied
}

// --- Name helpers ------------------------------------------------------------

// The rules behind these names live in internal/backup, shared with purge and
// export so all three agree on what a tenant's stores are called.

// databaseName returns the PostgreSQL database name for a tenant + app.
func databaseName(tenant *gentianov1alpha1.Tenant, appName string) string {
	return backup.DatabaseName(tenant, appName)
}

// databaseCRName returns the Kubernetes resource name for the CloudNativePG Database CR.
func databaseCRName(tenantName, appName string) string {
	return backup.CNPGDatabaseCR(tenantName, appName)
}

// roleUserName returns the PostgreSQL role/user name for a tenant + app.
func roleUserName(tenantName, appName string) string {
	return backup.PostgresRole(tenantName, appName)
}

func roleJobName(tenantName, appName string) string {
	return fmt.Sprintf("pg-role-%s-%s", tenantName, appName)
}
