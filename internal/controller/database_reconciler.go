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
	"github.com/gentian-org/gentian-os/internal/meta"
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
	"github.com/gentian-org/gentian-os/internal/kernel"
)

const (
	conditionDatabaseReady = "DatabaseReady"
	cnpgGroup              = "postgresql.cnpg.io"
	cnpgVersion            = "v1"
	cnpgDatabaseKind       = "Database"
	cnpgClusterName        = "postgres" // shared CloudNativePG Cluster in platform-kernel
	postgresAdminSecret    = "postgres-admin"
	databaseRequeueAfter   = 2 * time.Second
)

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
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
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
	kernelManaged := false

	for _, appName := range pgApps {
		dbName := databaseName(tenant, appName)
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: appName}, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}

		if appUsesCrossplaneDBInit(profile) {
			// app-default emits {app}-db-init after the App claim is sequenced (post-identity).
			// AppsReady tracks composition convergence; do not block tenant Phase=Ready.
			continue
		}
		kernelManaged = true

		roleJobDone, err := r.ensureRoleJob(ctx, tenant, nsName, dbName, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure role Job for app %s: %w", appName, err)
		}
		if !roleJobDone {
			allDone = false
			continue
		}

		// Step 2 — CloudNativePG Database CR (only after role exists)
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
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}

	reason := "Provisioned"
	message := "All PostgreSQL databases and roles are ready"
	if !kernelManaged {
		reason = "AppCompositionOwnsDatabase"
		message = "PostgreSQL is provisioned by app compositions"
	}

	r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionTrue, reason, message)
	return ctrl.Result{}, nil
}

// collectPostgresApps returns profile names of apps that require a per-tenant
// PostgreSQL database.
func (r *TenantReconciler) collectPostgresApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	var pgApps []string
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
			profile.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEnginePostgreSQL {
			pgApps = append(pgApps, app.Profile)
		}
	}
	return pgApps, nil
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
func makeRoleJob(tenant *gentianov1alpha1.Tenant, nsName, dbName, appName, rolePassword string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	roleName := roleUserName(tenant.Name, appName)
	container := psqlContainer("provision-role", buildRoleScript(dbName, roleName), nsName)
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

// psqlContainer returns a Container that runs a psql script via the postgres:16-alpine image.
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

func buildRoleScript(dbName, roleName string) string {
	// When ROLE_PW is provided (Inc 21a — Seeder enabled) the script applies
	// that exact password to the role via CREATE/ALTER ROLE WITH PASSWORD,
	// keeping the live PostgreSQL password in lockstep with OpenBao. When
	// unset, a random password is generated locally and discarded after Job
	// completion (legacy behaviour — apps without seeded OpenBao cannot
	// connect, but backends that only need the database to exist still work).
	return fmt.Sprintf(`set -euo pipefail
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
psql -c "ALTER ROLE \"%s\" SET search_path TO \"%s\", public;" postgres
echo "schema %s ensured"
echo "privileges granted"`, roleName, roleName, roleName, roleName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, dbName, dbName, roleName, roleName, dbName, dbName)
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

// databaseName returns the PostgreSQL database name for a tenant + app.
// Uses spec.isolation.databasePrefix if set, otherwise defaults to "{tenant}_".
func databaseName(tenant *gentianov1alpha1.Tenant, appName string) string {
	prefix := tenant.Name + "_"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.DatabasePrefix != "" {
		prefix = tenant.Spec.Isolation.DatabasePrefix
	}
	// Replace hyphens to satisfy PostgreSQL identifier rules
	safe := func(s string) string {
		result := make([]byte, len(s))
		for i := 0; i < len(s); i++ {
			if s[i] == '-' {
				result[i] = '_'
			} else {
				result[i] = s[i]
			}
		}
		return string(result)
	}
	return safe(prefix) + safe(appName)
}

// databaseCRName returns the Kubernetes resource name for the CloudNativePG Database CR.
func databaseCRName(tenantName, appName string) string {
	return fmt.Sprintf("db-%s-%s", tenantName, appName)
}

// roleUserName returns the PostgreSQL role/user name for a tenant + app.
func roleUserName(tenantName, appName string) string {
	return fmt.Sprintf("%s_%s", tenantName, appName)
}

func roleJobName(tenantName, appName string) string {
	return fmt.Sprintf("pg-role-%s-%s", tenantName, appName)
}
