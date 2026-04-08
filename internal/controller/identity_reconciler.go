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
"k8s.io/apimachinery/pkg/types"
ctrl "sigs.k8s.io/controller-runtime"
"sigs.k8s.io/controller-runtime/pkg/client"

gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
conditionIdentityReady   = "IdentityReady"
keycloakProvisionerImage = "curlimages/curl:8.7.1"
keycloakAdminSecret      = "keycloak-admin"
appLabel                 = "gentianos.io/app"
identityRequeueAfter     = 30 * time.Second
)

// ensureIdentity provisions a Keycloak realm and OIDC clients for the tenant.
// It creates idempotent Kubernetes Jobs in the kernel namespace that call the
// Keycloak Admin REST API. Returns a non-zero RequeueAfter while Jobs are pending.
func (r *TenantReconciler) ensureIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
realmName := keycloakRealmName(tenant)

oidcApps, err := r.collectOIDCApps(ctx, tenant)
if err != nil {
return ctrl.Result{}, err
}

if len(oidcApps) == 0 {
r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
"NoIdentityRequired", "No apps require identity provisioning")
return ctrl.Result{}, nil
}

realmDone, err := r.ensureRealmJob(ctx, tenant, realmName)
if err != nil {
return ctrl.Result{}, fmt.Errorf("ensure Keycloak realm Job: %w", err)
}
if !realmDone {
r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
"ProvisioningRealm", "Waiting for Keycloak realm Job to complete")
return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
}

allDone := true
for _, appName := range oidcApps {
done, err := r.ensureClientJob(ctx, tenant, realmName, appName)
if err != nil {
return ctrl.Result{}, fmt.Errorf("ensure Keycloak client Job for app %s: %w", appName, err)
}
if !done {
allDone = false
}
}

if !allDone {
r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
"ProvisioningClients", "Waiting for OIDC client Jobs to complete")
return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
}

r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
"Provisioned", "Keycloak realm and OIDC clients are ready")
return ctrl.Result{}, nil
}

// collectOIDCApps returns the profile names of apps in tenant.spec.apps that
// have kernelRequirements.identity.oidc enabled.
func (r *TenantReconciler) collectOIDCApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
var oidcApps []string
for _, app := range tenant.Spec.Apps {
profile := &gentianov1alpha1.AppProfile{}
if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
if errors.IsNotFound(err) {
continue // profile not yet installed; will retry on next reconcile
}
return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
}
if profile.Spec.KernelRequirements != nil &&
profile.Spec.KernelRequirements.Identity != nil &&
profile.Spec.KernelRequirements.Identity.OIDC {
oidcApps = append(oidcApps, app.Profile)
}
}
return oidcApps, nil
}

// ensureRealmJob creates the Keycloak realm Job if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureRealmJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
jobName := realmJobName(tenant.Name)
job := &batchv1.Job{}
err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
if errors.IsNotFound(err) {
return false, r.Create(ctx, makeRealmJob(tenant, realmName))
}
if err != nil {
return false, err
}
if jobIsFailed(job) {
prop := metav1.DeletePropagationBackground
_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
return false, nil // recreated on next reconcile
}
return jobIsComplete(job), nil
}

// ensureClientJob creates the OIDC client Job for one app if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName, appName string) (bool, error) {
jobName := clientJobName(tenant.Name, appName)
job := &batchv1.Job{}
err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
if errors.IsNotFound(err) {
return false, r.Create(ctx, makeClientJob(tenant, realmName, appName))
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

// deleteIdentity handles identity cleanup on tenant deletion.
// With DeletionPolicyDelete it creates a Job that removes the Keycloak realm
// (which cascades all clients and sessions). With DeletionPolicyRetain it is
// a no-op — the realm and user accounts are preserved.
func (r *TenantReconciler) deleteIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
return nil
}
realmName := keycloakRealmName(tenant)
jobName := realmDeleteJobName(tenant.Name)
existing := &batchv1.Job{}
err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
if err == nil {
return nil // already created
}
if !errors.IsNotFound(err) {
return err
}
return r.Create(ctx, makeRealmDeleteJob(tenant, realmName))
}

// --- Job constructors --------------------------------------------------------

func makeRealmJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
ttl := int32(3600)
return &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      realmJobName(tenant.Name),
Namespace: kernelNamespace,
Labels: map[string]string{
tenantLabel:    tenant.Name,
managedByLabel: managedByValue,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: &ttl,
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
keycloakContainer("provision-realm", buildRealmScript(realmName, tenant.Spec.DisplayName)),
},
},
},
},
}
}

func makeClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName string) *batchv1.Job {
ttl := int32(3600)
clientID := oidcClientID(tenant.Name, appName)
redirectURI := fmt.Sprintf("https://%s/%s/*", tenant.Spec.Domain, appName)
return &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      clientJobName(tenant.Name, appName),
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
keycloakContainer("provision-client", buildClientScript(realmName, clientID, redirectURI)),
},
},
},
},
}
}

func makeRealmDeleteJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
ttl := int32(3600)
return &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      realmDeleteJobName(tenant.Name),
Namespace: kernelNamespace,
Labels: map[string]string{
tenantLabel:    tenant.Name,
managedByLabel: managedByValue,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: &ttl,
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
keycloakContainer("delete-realm", buildRealmDeleteScript(realmName)),
},
},
},
},
}
}

// keycloakContainer returns a Container spec that runs a shell script via the
// curl-based Keycloak provisioner image. Credentials are injected from the
// well-known keycloak-admin Secret in the kernel namespace.
func keycloakContainer(name, script string) corev1.Container {
return corev1.Container{
Name:    name,
Image:   keycloakProvisionerImage,
Command: []string{"/bin/sh", "-c", script},
Env: []corev1.EnvVar{
{
Name: "KEYCLOAK_URL",
ValueFrom: &corev1.EnvVarSource{
SecretKeyRef: &corev1.SecretKeySelector{
LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
Key:                  "url",
},
},
},
{
Name: "KEYCLOAK_ADMIN_PASSWORD",
ValueFrom: &corev1.EnvVarSource{
SecretKeyRef: &corev1.SecretKeySelector{
LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
Key:                  "password",
},
},
},
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
					Key:                  "username",
				},
			},
		},
		},
	}
}

// --- Shell scripts -----------------------------------------------------------

func buildRealmScript(realmName, displayName string) string {
return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s")
if [ "${HTTP}" = "404" ]; then
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true,"displayName":"%s","registrationAllowed":false}'
  echo "realm %s created"
else
  echo "realm %s already exists (HTTP ${HTTP})"
fi`, realmName, realmName, displayName, realmName, realmName)
}

func buildClientScript(realmName, clientID, redirectURI string) string {
return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
COUNT=$(curl -sf \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/clients?clientId=%s" \
  | tr -cd '[' | wc -c | tr -d ' ')
if [ "${COUNT}" = "0" ]; then
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms/%s/clients" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"clientId":"%s","redirectUris":["%s"],"protocol":"openid-connect","standardFlowEnabled":true,"serviceAccountsEnabled":true,"publicClient":false}'
  echo "client %s created in realm %s"
else
  echo "client %s already exists in realm %s"
fi`, realmName, clientID, realmName, clientID, redirectURI, clientID, realmName, clientID, realmName)
}

func buildRealmDeleteScript(realmName string) string {
return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -X DELETE \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s")
echo "realm %s deletion requested (HTTP ${HTTP})"`, realmName, realmName)
}

// --- Name helpers ------------------------------------------------------------

// keycloakRealmName returns the Keycloak realm name for a tenant.
// Uses spec.isolation.keycloakRealm if set, otherwise defaults to the tenant name.
func keycloakRealmName(tenant *gentianov1alpha1.Tenant) string {
if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.KeycloakRealm != "" {
return tenant.Spec.Isolation.KeycloakRealm
}
return tenant.Name
}

func realmJobName(tenantName string) string {
return fmt.Sprintf("keycloak-realm-%s", tenantName)
}

func clientJobName(tenantName, appName string) string {
return fmt.Sprintf("keycloak-client-%s-%s", tenantName, appName)
}

func realmDeleteJobName(tenantName string) string {
return fmt.Sprintf("keycloak-realm-delete-%s", tenantName)
}

func oidcClientID(tenantName, appName string) string {
return fmt.Sprintf("%s-%s", tenantName, appName)
}

// --- Job status helpers ------------------------------------------------------

func jobIsComplete(job *batchv1.Job) bool {
for _, c := range job.Status.Conditions {
if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
return true
}
}
return false
}

func jobIsFailed(job *batchv1.Job) bool {
for _, c := range job.Status.Conditions {
if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
return true
}
}
return false
}
