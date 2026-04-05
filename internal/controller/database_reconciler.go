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
"time"

batchv1 "k8s.io/api/batch/v1"
corev1 "k8s.io/api/core/v1"
"k8s.io/apimachinery/pkg/api/errors"
metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
"k8s.io/apimachinery/pkg/runtime/schema"
"k8s.io/apimachinery/pkg/types"
ctrl "sigs.k8s.io/controller-runtime"
"sigs.k8s.io/controller-runtime/pkg/client"

gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
conditionDatabaseReady   = "DatabaseReady"
cnpgGroup                = "postgresql.cnpg.io"
cnpgVersion              = "v1"
cnpgDatabaseKind         = "Database"
cnpgClusterName          = "postgres" // shared CloudNativePG Cluster in platform-kernel
psqlProvisionerImage     = "bitnami/postgresql:16"
postgresAdminSecret      = "postgres-admin"
databaseRequeueAfter     = 30 * time.Second
)

var cnpgDatabaseGVR = schema.GroupVersionResource{
Group:    cnpgGroup,
Version:  cnpgVersion,
Resource: "databases",
}

// ensureDatabase provisions per-app-per-tenant PostgreSQL databases via
// CloudNativePG Database CRs and per-app role Jobs.
func (r *TenantReconciler) ensureDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
pgApps, err := r.collectPostgresApps(ctx, tenant)
if err != nil {
return ctrl.Result{}, err
}

if len(pgApps) == 0 {
r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionTrue,
"NoDatabaseRequired", "No apps require PostgreSQL provisioning")
return ctrl.Result{}, nil
}

nsName := tenantNamespaceName(tenant)
allDone := true

for _, appName := range pgApps {
dbName := databaseName(tenant, appName)

// Step 1 — CloudNativePG Database CR
dbReady, err := r.ensureDatabaseCR(ctx, tenant, nsName, dbName, appName)
if err != nil {
return ctrl.Result{}, fmt.Errorf("ensure Database CR for app %s: %w", appName, err)
}
if !dbReady {
allDone = false
continue
}

// Step 2 — psql role Job (only after Database CR is ready)
roleJobDone, err := r.ensureRoleJob(ctx, tenant, nsName, dbName, appName)
if err != nil {
return ctrl.Result{}, fmt.Errorf("ensure role Job for app %s: %w", appName, err)
}
if !roleJobDone {
allDone = false
}
}

if !allDone {
r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse,
"Provisioning", "Waiting for PostgreSQL databases and roles to be ready")
return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
}

r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionTrue,
"Provisioned", "All PostgreSQL databases and roles are ready")
return ctrl.Result{}, nil
}

// collectPostgresApps returns profile names of apps that require a PostgreSQL database.
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

// ensureDatabaseCR creates (or confirms the existence of) a CloudNativePG Database CR
// in the tenant namespace. Returns true once the Database CR reports Ready=True.
func (r *TenantReconciler) ensureDatabaseCR(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) (bool, error) {
desired := buildDatabaseCR(tenant, nsName, dbName, appName)
crName := databaseCRName(tenant.Name, appName)

existing := &unstructured.Unstructured{}
existing.SetGroupVersionKind(desired.GroupVersionKind())
err := r.Get(ctx, types.NamespacedName{Name: crName, Namespace: nsName}, existing)
if errors.IsNotFound(err) {
return false, r.Create(ctx, desired)
}
if err != nil {
return false, err
}
return cnpgDatabaseIsReady(existing), nil
}

// ensureRoleJob creates the psql role/user Job for the app if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureRoleJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) (bool, error) {
jobName := roleJobName(tenant.Name, appName)
job := &batchv1.Job{}
err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
if errors.IsNotFound(err) {
return false, r.Create(ctx, makeRoleJob(tenant, nsName, dbName, appName))
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

// deleteDatabase handles database cleanup on tenant deletion.
// DeletionPolicy=Delete: deletes the CloudNativePG Database CRs (operator drops the databases).
// DeletionPolicy=Retain: no-op — databases and data are preserved.
func (r *TenantReconciler) deleteDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
return nil
}
nsName := tenantNamespaceName(tenant)
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
profile.Spec.KernelRequirements.Database.Engine != gentianov1alpha1.DatabaseEnginePostgreSQL {
continue
}
crName := databaseCRName(tenant.Name, app.Profile)
obj := &unstructured.Unstructured{}
obj.SetGroupVersionKind(schema.GroupVersionKind{
Group:   cnpgGroup,
Version: cnpgVersion,
Kind:    cnpgDatabaseKind,
})
obj.SetName(crName)
obj.SetNamespace(nsName)
if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
return fmt.Errorf("delete Database CR %s: %w", crName, err)
}
}
return nil
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
obj.SetNamespace(nsName)
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
_ = unstructured.SetNestedField(obj.Object, true, "spec", "ensure")
}
return obj
}

// makeRoleJob creates a psql Job that creates the per-app PostgreSQL role
// and grants full privileges on the provisioned database.
func makeRoleJob(tenant *gentianov1alpha1.Tenant, nsName, dbName, appName string) *batchv1.Job {
ttl := int32(3600)
roleName := roleUserName(tenant.Name, appName)
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
Containers: []corev1.Container{
psqlContainer("provision-role", buildRoleScript(dbName, roleName), nsName),
},
},
},
},
}
}

// psqlContainer returns a Container that runs a psql script via the bitnami/postgresql image.
// Credentials are injected from the postgres-admin Secret in the kernel namespace.
func psqlContainer(name, script, tenantNamespace string) corev1.Container {
return corev1.Container{
Name:    name,
Image:   psqlProvisionerImage,
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
return fmt.Sprintf(`set -euo pipefail
ROLE_EXISTS=$(psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='%s'" postgres)
if [ "${ROLE_EXISTS}" != "1" ]; then
  ROLE_PW=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)
  psql -c "CREATE ROLE \"%s\" WITH LOGIN PASSWORD '${ROLE_PW}';" postgres
  echo "role %s created"
else
  echo "role %s already exists"
fi
DB_EXISTS=$(psql -tAc "SELECT 1 FROM pg_database WHERE datname='%s'" postgres)
if [ "${DB_EXISTS}" != "1" ]; then
  psql -c "CREATE DATABASE \"%s\" OWNER \"%s\";" postgres
  echo "database %s created"
fi
psql -c "GRANT ALL PRIVILEGES ON DATABASE \"%s\" TO \"%s\";" postgres
echo "privileges granted"`, roleName, roleName, roleName, roleName, dbName, dbName, roleName, dbName, dbName, roleName)
}

// --- Status helpers ----------------------------------------------------------

// cnpgDatabaseIsReady returns true when a CloudNativePG Database CR reports
// a "Ready" condition with status "True". Used to gate the role Job creation.
func cnpgDatabaseIsReady(obj *unstructured.Unstructured) bool {
conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
if err != nil || !found {
return false
}
for _, c := range conditions {
cond, ok := c.(map[string]interface{})
if !ok {
continue
}
if cond["type"] == "Ready" && cond["status"] == "True" {
return true
}
}
return false
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
