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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	// kernelLDAPAdminSecret is the name of the LDAP admin secret in kernelNamespace.
	// The nubusUmcServer chart uses this to bind to the shared LDAP directory.
	kernelLDAPAdminSecret = "nubus-dev-ldap-server-admin"

	// kernelNubusCredentials holds per-service DB passwords managed by the Nubus
	// Helm release (key: pg-authsession-password).
	kernelNubusCredentials = "nubus-credentials"

	// umcDBPasswordKey is the key inside kernelNubusCredentials for the
	// authSession PostgreSQL password.
	umcDBPasswordKey = "pg-authsession-password"

	// Per-tenant resource names (created in the tenant namespace).
	umcLDAPSecretName   = "umc-ldap-admin"
	umcDBSecretName     = "umc-db-credentials"
	umcUCRConfigMapName = "umc-ucr"
)

var (
	// Chart coordinates — overridable via operator env vars so upgrades do not
	// require an operator image rebuild.
	umcChartRepo    = envOrDefault("UMC_CHART_REPO", "oci://artifacts.software-univention.de/nubus/charts")
	umcChartName    = envOrDefault("UMC_CHART_NAME", "umc-server")
	umcChartVersion = envOrDefault("UMC_CHART_VERSION", "0.54.2")

	// Shared PostgreSQL connection — authSession database used by all per-tenant
	// UMC instances. UMC sessions are keyed by user DN (which includes the
	// tenant OU), so tenants do not share session state.
	umcDBHost = envOrDefault("UMC_DB_HOST", "opendesk-postgresql-dev.gentian-infra-dev.svc.cluster.local")
	umcDBPort = envOrDefault("UMC_DB_PORT", "5432")
	umcDBName = envOrDefault("UMC_DB_NAME", "nubus_authsession")
	umcDBUser = envOrDefault("UMC_DB_USER", "authsession_user")

	// Internal (in-cluster) Keycloak base URL.
	umcKeycloakInternalBase = envOrDefault("UMC_KEYCLOAK_INTERNAL_BASE", "http://nubus-dev-keycloak:8080")
)

// ensureUMC deploys a per-tenant nubusUmcServer instance that allows the
// tenant admin to manage their users at:
//
//	https://<tenant>.<domain>/univention/management/
//
// This is necessary because the shared Nubus portal is hardwired to the kernel
// Keycloak realm and cannot authenticate users from tenant-scoped LDAP OUs
// (Phase B LDAP restructuring). Each tenant gets their own UMC instance with
// UCR overrides pointing to their Keycloak realm and LDAP OU.
//
// Non-blocking: errors are logged and retried without blocking Phase=Ready.
func (r *TenantReconciler) ensureUMC(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	effectiveDomain := tenant.EffectiveDomain(r.KernelDomain)

	if err := r.ensureUMCLDAPSecret(ctx, tenant, nsName); err != nil {
		return fmt.Errorf("ensure UMC LDAP secret: %w", err)
	}
	if err := r.ensureUMCDBSecret(ctx, tenant, nsName); err != nil {
		return fmt.Errorf("ensure UMC DB secret: %w", err)
	}
	if err := r.ensureUMCConfigMap(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC UCR ConfigMap: %w", err)
	}
	if err := r.ensureUMCApplication(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC ArgoCD Application: %w", err)
	}
	return nil
}

// ensureUMCLDAPSecret replicates the kernel LDAP admin secret into the tenant
// namespace so the nubusUmcServer chart can bind to the shared LDAP directory.
// Soft-fails when the source Secret is not yet present (operator retries on the
// next reconcile).
func (r *TenantReconciler) ensureUMCLDAPSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kernelLDAPAdminSecret,
		Namespace: kernelNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read LDAP admin secret from %s: %w", kernelNamespace, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcLDAPSecretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": source.Data["password"]},
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: umcLDAPSecretName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = desired.Data
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureUMCDBSecret copies the PostgreSQL auth-session password from the shared
// nubus-credentials Secret into the tenant namespace for use by the
// nubusUmcServer chart.
func (r *TenantReconciler) ensureUMCDBSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kernelNubusCredentials,
		Namespace: kernelNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read nubus-credentials from %s: %w", kernelNamespace, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcDBSecretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": source.Data[umcDBPasswordKey]},
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: umcDBSecretName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = desired.Data
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureUMCConfigMap creates or updates the UCR override ConfigMap in the
// tenant namespace. The base-forced.conf values scope the per-tenant UMC to
// the correct Keycloak realm and LDAP OU.
func (r *TenantReconciler) ensureUMCConfigMap(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	// External Keycloak issuer URL (public, used for OIDC token verification).
	keycloakExternal := fmt.Sprintf("https://id.%s/realms/%s", r.KernelDomain, tenant.Name)
	// Internal Keycloak issuer URL (in-cluster, avoids hairpin NAT for token fetches).
	keycloakInternal := fmt.Sprintf("%s/realms/%s", umcKeycloakInternalBase, tenant.Name)
	// OIDC client ID registered in the tenant Keycloak realm by identity_reconciler.
	oidcClientID := fmt.Sprintf("https://%s/univention/oidc/", effectiveDomain)
	// Per-tenant LDAP base — scopes all directory operations to the tenant OU.
	tenantLDAPBase := fmt.Sprintf("ou=%s,%s", tenant.Name, r.LDAPBase)

	baseForcedConf := strings.Join([]string{
		"server/role: memberserver",
		fmt.Sprintf("ldap/master: %s", r.LDAPServer),
		"ldap/master/port: 389",
		fmt.Sprintf("ldap/hostdn: cn=admin,%s", r.LDAPBase),
		fmt.Sprintf("ldap/base: %s", tenantLDAPBase),
		"directory/manager/starttls: 0",
		fmt.Sprintf("umc/oidc/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/issuer-internal: %s", keycloakInternal),
		fmt.Sprintf("umc/oidc/nubus/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/nubus/client-id: %s", oidcClientID),
	}, "\n") + "\n"

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcUCRConfigMapName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Data: map[string]string{"base-forced.conf": baseForcedConf},
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: umcUCRConfigMapName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = desired.Data
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureUMCApplication creates (or checks) the ArgoCD Application CR that
// deploys the nubusUmcServer Helm chart into the tenant namespace. The
// Application is idempotent: if it already exists the function returns without
// modification (ArgoCD self-heals any drift).
func (r *TenantReconciler) ensureUMCApplication(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	appName := umcApplicationName(tenant.Name)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	err := r.Get(ctx, types.NamespacedName{Name: appName, Namespace: argocdNamespace}, obj)
	if errors.IsNotFound(err) {
		return r.Create(ctx, buildUMCApplication(tenant, nsName, effectiveDomain))
	}
	return err
}

// deleteUMC removes the ArgoCD Application CR for the per-tenant UMC on
// DeletionPolicy=Delete. ArgoCD prunes all resources it owns. The operator-
// managed Secrets and ConfigMap are cleaned up by the namespace deletion.
func (r *TenantReconciler) deleteUMC(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	appCR := &unstructured.Unstructured{}
	appCR.SetGroupVersionKind(argocdApplicationGVK)
	appCR.SetName(umcApplicationName(tenant.Name))
	appCR.SetNamespace(argocdNamespace)
	if err := r.Delete(ctx, appCR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete UMC Application CR: %w", err)
	}
	return nil
}

// buildUMCApplication constructs the ArgoCD Application CR that deploys the
// nubusUmcServer Helm chart in the tenant namespace.
func buildUMCApplication(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	obj.SetName(umcApplicationName(tenant.Name))
	obj.SetNamespace(argocdNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(obj.Object, umcChartRepo, "spec", "source", "repoURL")
	_ = unstructured.SetNestedField(obj.Object, umcChartName, "spec", "source", "chart")
	_ = unstructured.SetNestedField(obj.Object, umcChartVersion, "spec", "source", "targetRevision")
	_ = unstructured.SetNestedField(obj.Object, buildUMCHelmValues(effectiveDomain), "spec", "source", "helm", "values")
	_ = unstructured.SetNestedField(obj.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "destination", "namespace")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "prune")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "selfHeal")
	return obj
}

// buildUMCHelmValues returns the YAML Helm values string for the per-tenant
// nubusUmcServer deployment.
func buildUMCHelmValues(effectiveDomain string) string {
	return fmt.Sprintf(`global:
  configMapUcr: %s
selfService:
  enabled: false
ldap:
  auth:
    existingSecret:
      name: %s
      keyMapping:
        password: password
postgresql:
  authSession:
    connection:
      host: %s
      port: "%s"
    auth:
      database: %s
      username: %s
      existingSecret:
        name: %s
        keyMapping:
          password: password
ingress:
  enabled: true
  host: %s
  ingressClassName: nginx
  certManager:
    enabled: false
  tls:
    enabled: true
    secretName: %s
`,
		umcUCRConfigMapName,
		umcLDAPSecretName,
		umcDBHost,
		umcDBPort,
		umcDBName,
		umcDBUser,
		umcDBSecretName,
		effectiveDomain,
		kernelWildcardTenantSecret,
	)
}

// umcApplicationName returns the ArgoCD Application CR name for a tenant's
// per-tenant UMC instance.
func umcApplicationName(tenantName string) string {
	return fmt.Sprintf("umc-%s", tenantName)
}
