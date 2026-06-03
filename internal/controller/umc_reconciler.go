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
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	umcLDAPSecretName          = "umc-ldap-admin"
	umcDBSecretName            = "umc-db-credentials"
	umcDBSelfServiceSecretName = "umc-db-selfservice"
	umcSMTPSecretName          = "umc-smtp"
	umcOIDCSecretName          = "umc-oidc-client"
	umcUCRConfigMapName        = "umc-ucr"
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
	umcGatewayChartName = envOrDefault("UMC_GATEWAY_CHART_NAME", "umc-gateway")

	// gentian-ui portal-frontend image (same image as kernel gentian-login /
	// portal-frontend). Serves the branded Gentian login shell on each tenant
	// domain at /login.
	gentianPortalFrontendRegistry = envOrDefault("GENTIAN_PORTAL_FRONTEND_REGISTRY", "ghcr.io")
	gentianPortalFrontendRepo       = envOrDefault("GENTIAN_PORTAL_FRONTEND_REPO", "gentian-org/portal-frontend")
	gentianPortalFrontendTag        = envOrDefault("GENTIAN_PORTAL_FRONTEND_TAG", "0855185a")

	// Shared PostgreSQL connection — authSession database used by all per-tenant
	// UMC instances. UMC sessions are keyed by user DN (which includes the
	// tenant OU), so tenants do not share session state.
	umcDBHost = envOrDefault("UMC_DB_HOST", "opendesk-postgresql-dev.gentian-infra-dev.svc.cluster.local")
	umcDBPort = envOrDefault("UMC_DB_PORT", "5432")
	umcDBName = envOrDefault("UMC_DB_NAME", "nubus_authsession")
	umcDBUser = envOrDefault("UMC_DB_USER", "authsession_user")

	// Internal (in-cluster) Keycloak base URL. Default uses the FQDN derived
	// from servicesNamespace so UMC pods in tenant namespaces can reach it.
	umcKeycloakInternalBase = envOrDefault("UMC_KEYCLOAK_INTERNAL_BASE",
		fmt.Sprintf("http://nubus-dev-keycloak.%s.svc.cluster.local:8080", servicesNamespace))

	// LDAP server hostname (without scheme/port) for the UCR ldap/server/name
	// key consumed by SSSD's getLDAPURIs() helper. Must be the plain hostname,
	// not a URL — SSSD builds ldap://<host>:<port> itself.
	umcLDAPServerName = envOrDefault("UMC_LDAP_SERVER_NAME",
		fmt.Sprintf("nubus-dev-ldap-server.%s.svc.cluster.local", servicesNamespace))
)

// ensureUMC deploys per-tenant UMC stack on the tenant effective domain:
//
//   /login/                          → gentian-ui portal-frontend (branded login)
//   /univention/management/, /js/    → nubus umc-gateway (dojo UMC shell)
//   /univention/(auth|oidc|get|…)/   → nubus umc-server (API)
//
// The shared kernel portal (portal.<kernel-domain>) uses the kernel Keycloak
// realm and cannot authenticate tenant-scoped LDAP users.
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
	if err := r.ensureUMCGatewayRelease(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure UMC gateway Release: %w", err)
	}
	if err := r.ensureUMCGentianLogin(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure gentian-ui login frontend: %w", err)
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

	// SAML IdP descriptor URLs — the external URL is stored in the UCR key
	// (consumed by UMC's sp.py) while the internal URL is used only at
	// container startup to download the metadata without hairpin NAT.
	samlIDPExternal := fmt.Sprintf("https://id.%s/realms/%s/protocol/saml/descriptor", r.KernelDomain, tenant.Name)
	samlIDPInternal := fmt.Sprintf("%s/protocol/saml/descriptor", keycloakInternal)

	baseForcedConf := strings.Join(append([]string{
		"server/role: memberserver",
		// hostname + domainname are required by SSSD's UCR template (sssd.conf
		// domain section) and by UMC's saml/sp.py which builds the cert path as
		// /etc/univention/ssl/<hostname>.<domainname>/cert.pem.
		fmt.Sprintf("hostname: %s", tenant.Name),
		fmt.Sprintf("domainname: %s", r.KernelDomain),
		fmt.Sprintf("ldap/master: %s", r.LDAPServer),
		"ldap/master/port: 389",
		// ldap/server/name is the plain hostname (no scheme/port) consumed by
		// SSSD's getLDAPURIs() helper to build ldap_uri in sssd.conf.
		fmt.Sprintf("ldap/server/name: %s", umcLDAPServerName),
		"ldap/server/port: 389",
		fmt.Sprintf("ldap/hostdn: cn=admin,%s", r.LDAPBase),
		fmt.Sprintf("ldap/base: %s", tenantLDAPBase),
		"directory/manager/starttls: 0",
		// SAML SP: the entrypoint script (50-entrypoint.sh) reads these to
		// download the IdP metadata and to create the cert symlink at
		// /etc/univention/ssl/<sp-server>/cert.pem.  sp-server must equal
		// <hostname>.<domainname> so that saml/sp.py finds the symlinked cert.
		fmt.Sprintf("umc/saml/sp-server: %s", effectiveDomain),
		fmt.Sprintf("umc/saml/idp-server: %s", samlIDPExternal),
		fmt.Sprintf("umc/saml/idp-server-internal: %s", samlIDPInternal),
		fmt.Sprintf("umc/oidc/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/issuer-internal: %s", keycloakInternal),
		fmt.Sprintf("umc/oidc/nubus/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/nubus/client-id: %s", oidcClientID),
		// UMC's Tornado HTTP server defaults to binding on 127.0.0.1. The
		// Kubernetes liveness/readiness probes connect to the pod IP, so UMC
		// must bind on all interfaces for the probes (and the Traefik proxy
		// pod) to reach it.
		"umc/http/interface: 0.0.0.0",
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
		Data: map[string]string{
			"base.conf":          baseForcedConf,
			"base-defaults.conf": "",
		},
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
	desired := buildUMCRelease(tenant, nsName, effectiveDomain, r.KernelDomain)

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
	gwCR := &unstructured.Unstructured{}
	gwCR.SetGroupVersionKind(helmReleaseGVK)
	gwCR.SetName(umcGatewayReleaseName(tenant.Name))
	if err := r.Delete(ctx, gwCR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete UMC gateway Release CR: %w", err)
	}
	if err := r.deleteUMCGentianLogin(ctx, tenant, tenantNamespaceName(tenant)); err != nil {
		return err
	}
	return nil
}

func (r *TenantReconciler) ensureUMCGatewayRelease(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	desired := buildUMCGatewayRelease(tenant, nsName, effectiveDomain, r.KernelDomain)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(helmReleaseGVK)
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName()}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Object["spec"] = desired.Object["spec"]
	return r.Patch(ctx, existing, patch)
}

// buildUMCRelease constructs the Crossplane provider-helm Release CR that
// deploys the nubusUmcServer Helm chart in the tenant namespace.
func buildUMCRelease(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain, kernelDomain string) *unstructured.Unstructured {
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
	_ = unstructured.SetNestedMap(obj.Object, buildUMCHelmValues(effectiveDomain, kernelDomain), "spec", "forProvider", "values")
	_ = unstructured.SetNestedField(obj.Object, "kubernetes", "spec", "providerConfigRef", "name")
	return obj
}

// buildUMCHelmValues returns the Helm values map for the per-tenant
// nubusUmcServer deployment, structured as a map[string]interface{} for
// provider-helm Release spec.forProvider.values (JSON object type).
func buildUMCHelmValues(effectiveDomain, kernelDomain string) map[string]interface{} {
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
			"ingressClassName": "public",
			"certManager": map[string]interface{}{
				"enabled": false,
			},
			"tls": map[string]interface{}{
				"enabled":    true,
				"secretName": kernelWildcardTenantSecret,
			},
			"annotations": umcIngressAnnotations(effectiveDomain, kernelDomain),
			"paths": []interface{}{
				map[string]interface{}{
					// API paths only — static UMC shell (HTML/JS/CSS) is served by
					// umc-gateway; gentian-ui portal-frontend covers /login branding.
					"path":     `/(univention)/(auth|logout|oidc|get|set|command|upload)(.*)$`,
					"pathType": "ImplementationSpecific",
				},
			},
		},
	}
}

func umcIngressAnnotations(effectiveDomain, kernelDomain string) map[string]interface{} {
	corsOrigin := fmt.Sprintf("https://%s, https://id.%s", effectiveDomain, kernelDomain)
	return map[string]interface{}{
		"nginx.ingress.kubernetes.io/use-regex":          "true",
		"nginx.ingress.kubernetes.io/ssl-redirect":       "false",
		"nginx.ingress.kubernetes.io/force-ssl-redirect":   "false",
		"nginx.ingress.kubernetes.io/enable-cors":        "true",
		"nginx.ingress.kubernetes.io/cors-allow-origin":  corsOrigin,
	}
}

func umcIngressAnnotationStrings(effectiveDomain, kernelDomain string) map[string]string {
	raw := umcIngressAnnotations(effectiveDomain, kernelDomain)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func buildUMCGatewayRelease(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain, kernelDomain string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(helmReleaseGVK)
	obj.SetName(umcGatewayReleaseName(tenant.Name))
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, umcChartRepo, "spec", "forProvider", "chart", "repository")
	_ = unstructured.SetNestedField(obj.Object, umcGatewayChartName, "spec", "forProvider", "chart", "name")
	_ = unstructured.SetNestedField(obj.Object, umcChartVersion, "spec", "forProvider", "chart", "version")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "forProvider", "namespace")
	_ = unstructured.SetNestedField(obj.Object, false, "spec", "forProvider", "wait")
	_ = unstructured.SetNestedMap(obj.Object, buildUMCGatewayHelmValues(effectiveDomain, kernelDomain), "spec", "forProvider", "values")
	_ = unstructured.SetNestedField(obj.Object, "kubernetes", "spec", "providerConfigRef", "name")
	return obj
}

func buildUMCGatewayHelmValues(effectiveDomain, kernelDomain string) map[string]interface{} {
	portalSub, domain := umcChartPortalDomain(effectiveDomain, kernelDomain)
	return map[string]interface{}{
		"global": map[string]interface{}{
			"configMapUcr": umcUCRConfigMapName,
			// umc-gateway hardcodes ingress CORS as
			// https://<subDomains.portal>.<domain>, https://id.<domain>.
			"domain": domain,
			"subDomains": map[string]interface{}{
				"portal":   portalSub,
				"keycloak": "id",
			},
		},
		"ingress": map[string]interface{}{
			"enabled":          true,
			"host":             effectiveDomain,
			"ingressClassName": "public",
			"certManager": map[string]interface{}{
				"enabled": false,
			},
			"tls": map[string]interface{}{
				"enabled":    true,
				"secretName": kernelWildcardTenantSecret,
			},
			"annotations": umcIngressAnnotations(effectiveDomain, kernelDomain),
		},
	}
}

// umcReleaseName returns the Crossplane Release CR name for a tenant's
// per-tenant UMC instance.
func umcReleaseName(tenantName string) string {
	return fmt.Sprintf("umc-%s", tenantName)
}

func umcGatewayReleaseName(tenantName string) string {
	return fmt.Sprintf("umc-gateway-%s", tenantName)
}

// umcChartPortalDomain derives global.domain and global.subDomains.portal for
// nubus umc-gateway Helm charts. The chart template hardcodes CORS as
// https://<portal>.<domain>, https://id.<domain> — user-supplied ingress
// annotations cannot override it.
func umcChartPortalDomain(effectiveDomain, kernelDomain string) (portalSubdomain, domain string) {
	if effectiveDomain == "" {
		return "", kernelDomain
	}
	if kernelDomain != "" && strings.HasSuffix(effectiveDomain, "."+kernelDomain) {
		return strings.TrimSuffix(effectiveDomain, "."+kernelDomain), kernelDomain
	}
	if dot := strings.IndexByte(effectiveDomain, '.'); dot > 0 {
		return effectiveDomain[:dot], effectiveDomain[dot+1:]
	}
	return effectiveDomain, kernelDomain
}

func umcGentianLoginName(tenantName string) string {
	return fmt.Sprintf("umc-%s-gentian-login", tenantName)
}

type gentianLoginBranding struct {
	Name            string `json:"name"`
	Tagline         string `json:"tagline"`
	LogoURL         string `json:"logoUrl"`
	ThemeColor      string `json:"themeColor"`
	AccentColor     string `json:"accentColor"`
	BackgroundColor string `json:"backgroundColor"`
	CardColor       string `json:"cardColor"`
	OrgName         string `json:"orgName"`
}

func (r *TenantReconciler) ensureUMCGentianLogin(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	name := umcGentianLoginName(tenant.Name)
	brandingName := name + "-branding"
	displayName := tenant.Spec.DisplayName
	if displayName == "" {
		displayName = tenant.Name
	}
	brandingJSON, err := json.Marshal(gentianLoginBranding{
		Name:            displayName,
		Tagline:         "Sign in to manage your tenant",
		LogoURL:         "/favicon/gentian-logo.png",
		ThemeColor:      "#262696",
		AccentColor:     "#4A4AB3",
		BackgroundColor: "#F4F1EA",
		CardColor:       "#FFFFFF",
		OrgName:         displayName,
	})
	if err != nil {
		return fmt.Errorf("marshal gentian login branding: %w", err)
	}

	desiredCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      brandingName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				"app.kubernetes.io/name": "gentian-login",
			},
		},
		Data: map[string]string{"branding.json": string(brandingJSON)},
	}
	existingCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: brandingName, Namespace: nsName}, existingCM); errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredCM); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepEqual(existingCM.Data, desiredCM.Data) {
		patch := client.MergeFrom(existingCM.DeepCopy())
		existingCM.Data = desiredCM.Data
		if err := r.Patch(ctx, existingCM, patch); err != nil {
			return err
		}
	}

	image := fmt.Sprintf("%s/%s:%s", gentianPortalFrontendRegistry, gentianPortalFrontendRepo, gentianPortalFrontendTag)
	replicas := int32(1)
	labels := map[string]string{
		tenantLabel:                tenant.Name,
		managedByLabel:             managedByValue,
		"app.kubernetes.io/name":   "gentian-login",
		"app.kubernetes.io/instance": name,
	}
	desiredDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "gentian-login",
					"app.kubernetes.io/instance": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "portal-frontend",
						Image: image,
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/login/", Port: intstrFromName("http")},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/login/", Port: intstrFromName("http")},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("20m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "branding",
							MountPath: "/var/www/html/branding.json",
							SubPath:   "branding.json",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "branding",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: brandingName},
							},
						},
					}},
				},
			},
		},
	}

	existingDeploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existingDeploy); errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredDeploy); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepEqual(existingDeploy.Spec, desiredDeploy.Spec) {
		patch := client.MergeFrom(existingDeploy.DeepCopy())
		existingDeploy.Spec = desiredDeploy.Spec
		if err := r.Patch(ctx, existingDeploy, patch); err != nil {
			return err
		}
	}

	desiredSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name":     "gentian-login",
				"app.kubernetes.io/instance": name,
			},
			Ports: []corev1.ServicePort{{
				Name: "http",
				Port: 80,
				TargetPort: intstrFromName("http"),
			}},
		},
	}
	existingSvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existingSvc); errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredSvc); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepEqual(existingSvc.Spec, desiredSvc.Spec) {
		patch := client.MergeFrom(existingSvc.DeepCopy())
		existingSvc.Spec = desiredSvc.Spec
		if err := r.Patch(ctx, existingSvc, patch); err != nil {
			return err
		}
	}

	pathTypePrefix := networkingv1.PathTypePrefix
	pathTypeExact := networkingv1.PathTypeExact
	ingressClass := "public"
	ingressAnn := umcIngressAnnotationStrings(effectiveDomain, r.KernelDomain)
	desiredIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   nsName,
			Labels:      labels,
			Annotations: ingressAnn,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{effectiveDomain},
				SecretName: kernelWildcardTenantSecret,
			}},
			Rules: []networkingv1.IngressRule{{
				Host: effectiveDomain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/login", PathType: &pathTypePrefix, Backend: ingressBackend(name)},
							{Path: "/css", PathType: &pathTypePrefix, Backend: ingressBackend(name)},
							{Path: "/fonts", PathType: &pathTypePrefix, Backend: ingressBackend(name)},
							{Path: "/sw.js", PathType: &pathTypeExact, Backend: ingressBackend(name)},
						},
					},
				},
			}},
		},
	}
	existingIngress := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existingIngress); errors.IsNotFound(err) {
		return r.Create(ctx, desiredIngress)
	}
	if err != nil {
		return err
	}
	if !ingressSpecEqual(existingIngress, desiredIngress) {
		patch := client.MergeFrom(existingIngress.DeepCopy())
		existingIngress.Spec = desiredIngress.Spec
		existingIngress.Annotations = desiredIngress.Annotations
		return r.Patch(ctx, existingIngress, patch)
	}
	return nil
}

func (r *TenantReconciler) deleteUMCGentianLogin(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	name := umcGentianLoginName(tenant.Name)
	for _, obj := range []client.Object{
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-branding", Namespace: nsName}},
	} {
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func ingressBackend(serviceName string) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: serviceName,
			Port: networkingv1.ServiceBackendPort{Name: "http"},
		},
	}
}

func intstrFromName(name string) intstr.IntOrString {
	return intstr.IntOrString{Type: intstr.String, StrVal: name}
}

func ingressSpecEqual(a, b *networkingv1.Ingress) bool {
	return equality.Semantic.DeepEqual(a.Spec, b.Spec) &&
		equality.Semantic.DeepEqual(a.Annotations, b.Annotations)
}
