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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	// kernelLDAPAdminSecret is the name of the LDAP admin secret in
	// servicesNamespace. The nubusUmcServer chart uses this to bind to the
	// shared LDAP directory.
	kernelLDAPAdminSecret = "nubus-dev-ldap-server-admin"

	// kernelNubusCredentials holds per-service DB passwords managed by the Nubus
	// Helm release (keys: pg-authsession-password, pg-selfservice-password, …).
	kernelNubusCredentials = "nubus-credentials"

	// umcDBPasswordKey is the key inside kernelNubusCredentials for the
	// authSession PostgreSQL password.
	umcDBPasswordKey = "pg-authsession-password"

	// umcDBSelfServicePasswordKey is the key inside kernelNubusCredentials for
	// the selfservice PostgreSQL password.
	umcDBSelfServicePasswordKey = "pg-selfservice-password"

	// Per-tenant resource names (created in the tenant namespace).
	umcLDAPSecretName            = "umc-ldap-admin"
	umcDBSecretName              = "umc-db-credentials"
	umcDBSelfServiceSecretName   = "umc-db-selfservice"
	umcSMTPSecretName            = "umc-smtp"
	umcOIDCSecretName            = "umc-oidc-client"
	umcUCRConfigMapName          = "umc-ucr"
)

// helmReleaseGVK is the GVK for the Crossplane provider-helm Release CR.
// The operator creates one Release per tenant to deploy the nubusUmcServer
// chart directly from the OCI registry (provider-helm uses the Go Helm SDK
// which supports anonymous OCI pulls without a prior registry login, unlike
// the ArgoCD repo-server).
var helmReleaseGVK = schema.GroupVersionKind{
	Group:   "helm.crossplane.io",
	Version: "v1beta1",
	Kind:    "Release",
}

var (
	// Chart coordinates — overridable via operator env vars so upgrades do not
	// require an operator image rebuild.
	// umcChartRepo is the full OCI repository URL for the umc-server chart,
	// including the chart name (provider-helm Release format).
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
	if err := r.ensureUMCPostgresSelfServiceSecret(ctx, tenant, nsName); err != nil {
		return fmt.Errorf("ensure UMC selfservice DB secret: %w", err)
	}
	if err := r.ensureUMCOIDCSecret(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC OIDC secret: %w", err)
	}
	if err := r.ensureUMCSMTPSecret(ctx, tenant, nsName); err != nil {
		return fmt.Errorf("ensure UMC SMTP secret: %w", err)
	}
	if err := r.ensureUMCConfigMap(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC UCR ConfigMap: %w", err)
	}
	if err := r.ensureUMCRelease(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC Helm Release: %w", err)
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
		Namespace: servicesNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read LDAP admin secret from %s: %w", servicesNamespace, err)
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
		Namespace: servicesNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read nubus-credentials from %s: %w", servicesNamespace, err)
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

// ensureUMCPostgresSelfServiceSecret copies the PostgreSQL selfservice password
// from the shared nubus-credentials Secret into the tenant namespace. The
// nubusUmcServer chart requires this even when selfService is disabled because
// the secret template has a hard required() guard.
func (r *TenantReconciler) ensureUMCPostgresSelfServiceSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kernelNubusCredentials,
		Namespace: servicesNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read nubus-credentials from %s: %w", servicesNamespace, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcDBSelfServiceSecretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": source.Data[umcDBSelfServicePasswordKey]},
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: umcDBSelfServiceSecretName, Namespace: nsName}, existing)
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

// ensureUMCOIDCSecret derives the per-tenant UMC OIDC client secret via the
// Seeder (same deterministic derivation used by the realm job) and stores it
// in the tenant namespace for use by the nubusUmcServer chart.
// Soft-fails when Seeder is unavailable (envtest / staged rollout).
func (r *TenantReconciler) ensureUMCOIDCSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	if r.Seeder == nil {
		// Without a Seeder we cannot derive the OIDC client secret. UMC will
		// not work until a secret is created manually or Seeder is configured.
		return nil
	}
	oidcClientIDValue := fmt.Sprintf("https://%s/univention/oidc/", effectiveDomain)
	issuer := fmt.Sprintf("https://id.%s/realms/%s", r.KernelDomain, tenant.Name)
	creds, err := r.Seeder.SeedOIDC(ctx, tenant.Name, "umc", issuer, oidcClientIDValue)
	if err != nil {
		return fmt.Errorf("seed UMC OIDC client secret: %w", err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcOIDCSecretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte(creds.ClientSecret)},
	}

	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: umcOIDCSecretName, Namespace: nsName}, existing)
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

// ensureUMCSMTPSecret copies the per-tenant SMTP password into a dedicated
// Secret in the tenant namespace for use by the nubusUmcServer chart.
// Soft-fails when the source smtp-credentials Secret is not yet present.
func (r *TenantReconciler) ensureUMCSMTPSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      smtpCredentialsSecretName(tenant.Name),
		Namespace: nsName,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil // not yet created; retry on next reconcile
		}
		return fmt.Errorf("read SMTP credentials secret: %w", err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcSMTPSecretName,
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
	err := r.Get(ctx, types.NamespacedName{Name: umcSMTPSecretName, Namespace: nsName}, existing)
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

	// Read per-tenant SMTP credentials to populate the UCR mail server keys.
	// Soft-fail: if the secret is not yet present the SMTP UCR keys are omitted
	// (UMC will use defaults) and the reconcile retries on the next cycle.
	smtpHost, smtpPort, smtpUser := "", "", ""
	smtpSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      smtpCredentialsSecretName(tenant.Name),
		Namespace: nsName,
	}, smtpSec); err == nil {
		smtpHost = string(smtpSec.Data["host"])
		smtpPort = string(smtpSec.Data["port"])
		smtpUser = string(smtpSec.Data["username"])
	}

	smtpLines := []string{}
	if smtpHost != "" {
		smtpLines = []string{
			fmt.Sprintf("umc/self-service/passwordreset/email/server: %s", smtpHost),
			fmt.Sprintf("umc/self-service/passwordreset/email/server/port: %s", smtpPort),
			"umc/self-service/passwordreset/email/server/starttls: false",
			fmt.Sprintf("umc/self-service/passwordreset/email/server/user: %s", smtpUser),
		}
	}

	baseForcedConf := strings.Join(append([]string{
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
	}, smtpLines...), "\n") + "\n"

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcUCRConfigMapName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Data: map[string]string{"base.conf": baseForcedConf},
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

// ensureUMCRelease creates (or checks) the Crossplane provider-helm Release CR
// that deploys the nubusUmcServer Helm chart into the tenant namespace.
// provider-helm uses the Go Helm SDK which supports anonymous OCI pulls from
// artifacts.software-univention.de without a prior registry login, avoiding
// the ArgoCD repo-server OCI credential matching quirk.
func (r *TenantReconciler) ensureUMCRelease(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	desired := buildUMCRelease(tenant, nsName, effectiveDomain)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(helmReleaseGVK)
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName()}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Patch spec to keep chart version and values current.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Object["spec"] = desired.Object["spec"]
	return r.Patch(ctx, existing, patch)
}

// deleteUMC removes the Crossplane Release CR for the per-tenant UMC on
// DeletionPolicy=Delete. provider-helm prunes all resources it owns. The
// operator-managed Secrets and ConfigMap are cleaned up by the namespace
// deletion.
func (r *TenantReconciler) deleteUMC(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	relCR := &unstructured.Unstructured{}
	relCR.SetGroupVersionKind(helmReleaseGVK)
	relCR.SetName(umcReleaseName(tenant.Name))
	if err := r.Delete(ctx, relCR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete UMC Release CR: %w", err)
	}
	return nil
}

// buildUMCRelease constructs the Crossplane provider-helm Release CR that
// deploys the nubusUmcServer Helm chart in the tenant namespace.
// The Release is cluster-scoped; spec.forProvider.namespace targets the tenant
// namespace.
func buildUMCRelease(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(helmReleaseGVK)
	obj.SetName(umcReleaseName(tenant.Name))
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	// chart.repository for provider-helm is the OCI registry path without the
	// chart name; provider-helm appends chart.name to form the full OCI ref.
	_ = unstructured.SetNestedField(obj.Object, umcChartRepo, "spec", "forProvider", "chart", "repository")
	_ = unstructured.SetNestedField(obj.Object, umcChartName, "spec", "forProvider", "chart", "name")
	_ = unstructured.SetNestedField(obj.Object, umcChartVersion, "spec", "forProvider", "chart", "version")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "forProvider", "namespace")
	_ = unstructured.SetNestedField(obj.Object, false, "spec", "forProvider", "wait")
	_ = unstructured.SetNestedMap(obj.Object, buildUMCHelmValues(effectiveDomain), "spec", "forProvider", "values")
	_ = unstructured.SetNestedField(obj.Object, "kubernetes", "spec", "providerConfigRef", "name")
	return obj
}

// buildUMCHelmValues returns the Helm values map for the per-tenant
// nubusUmcServer deployment, structured as a map[string]interface{} for
// provider-helm Release spec.forProvider.values (JSON object type).
func buildUMCHelmValues(effectiveDomain string) map[string]interface{} {
	return map[string]interface{}{
		"global": map[string]interface{}{
			"configMapUcr": umcUCRConfigMapName,
		},
		"selfService": map[string]interface{}{
			"enabled": false,
		},
		"ldap": map[string]interface{}{
			"auth": map[string]interface{}{
				"existingSecret": map[string]interface{}{
					"name": umcLDAPSecretName,
					"keyMapping": map[string]interface{}{
						"password": "password",
					},
				},
			},
		},
		"postgresql": map[string]interface{}{
			"authSession": map[string]interface{}{
				"connection": map[string]interface{}{
					"host": umcDBHost,
					"port": umcDBPort,
				},
				"auth": map[string]interface{}{
					"database": umcDBName,
					"username": umcDBUser,
					"existingSecret": map[string]interface{}{
						"name": umcDBSecretName,
						"keyMapping": map[string]interface{}{
							"password": "password",
						},
					},
				},
			},
			"selfservice": map[string]interface{}{
				"auth": map[string]interface{}{
					"existingSecret": map[string]interface{}{
						"name": umcDBSelfServiceSecretName,
						"keyMapping": map[string]interface{}{
							"password": "password",
						},
					},
				},
			},
		},
		"smtp": map[string]interface{}{
			"auth": map[string]interface{}{
				"existingSecret": map[string]interface{}{
					"name": umcSMTPSecretName,
					"keyMapping": map[string]interface{}{
						"password": "password",
					},
				},
			},
		},
		"umcServer": map[string]interface{}{
			"oidcClient": map[string]interface{}{
				"auth": map[string]interface{}{
					"existingSecret": map[string]interface{}{
						"name": umcOIDCSecretName,
						"keyMapping": map[string]interface{}{
							"password": "password",
						},
					},
				},
			},
		},
		"ingress": map[string]interface{}{
			"enabled":          true,
			"host":             effectiveDomain,
			"ingressClassName": "nginx",
			"certManager": map[string]interface{}{
				"enabled": false,
			},
			"tls": map[string]interface{}{
				"enabled":    true,
				"secretName": kernelWildcardTenantSecret,
			},
		},
	}
}

// umcReleaseName returns the Crossplane Release CR name for a tenant's
// per-tenant UMC instance.
func umcReleaseName(tenantName string) string {
	return fmt.Sprintf("umc-%s", tenantName)
}
