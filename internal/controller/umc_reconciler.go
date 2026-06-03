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
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	umcLDAPSecretName          = "umc-ldap-admin"
	umcDBSecretName            = "umc-db-credentials"
	umcDBSelfServiceSecretName = "umc-db-selfservice"
	umcSMTPSecretName          = "umc-smtp"
	umcOIDCSecretName          = "umc-oidc-client"
	umcUCRConfigMapName        = "umc-ucr"

	reloaderAutoAnnotation  = "reloader.stakater.com/auto"
	reloaderMatchAnnotation = "reloader.stakater.com/match"

	// umc-gateway reads base-defaults.conf via UCR; an empty file breaks Apache
	// LogLevel generation (see kernel nubus-dev-stack-data-ums-ucr).
	umcUCRBaseDefaultsConf = "# This file is empty on purpose\n# And needs to have at least two lines\n"
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

	// Post-login destination for tenant-domain entry redirects (/ and /login).
	// Nubus LoginDialog sends authenticated users here; matches kernel portal
	// login tile link target (/univention/login/?location=/univention/portal/).
	// Per-tenant portal UI is not provisioned yet — management console is the
	// tenant-admin entry point on the tenant effective domain.
	nubusTenantLoginLocation = "/univention/management/"

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

// ensureUMC deploys per-tenant Nubus UMC stack on the tenant effective domain:
//
//   /, /login                        → redirect to /univention/login/ (Nubus LoginDialog)
//   /univention/login/               → umc-gateway (Nubus login UI, not raw Keycloak)
//   /univention/management/, /js/    → umc-gateway (dojo UMC shell)
//   /univention/(auth|oidc|get|…)/   → umc-server (API; OIDC backend only)
//
// Keycloak remains the IdP behind Nubus OIDC — users must never be sent to
// id.<kernel-domain> directly from tenant ingress. The kernel portal at
// portal.<kernel-domain> uses the kernel realm and cannot authenticate
// tenant-scoped LDAP users.
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
	if err := r.deleteUMCLegacyGentianLogin(ctx, tenant, nsName); err != nil {
		return fmt.Errorf("remove legacy gentian-ui login frontend: %w", err)
	}
	if err := r.ensureUMCNubusLoginRedirects(ctx, tenant, nsName, effectiveDomain); err != nil {
		return fmt.Errorf("ensure Nubus login redirects: %w", err)
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

	baseForcedConf := strings.Join(append(append([]string{
		"server/role: memberserver",
		// hostname + domainname are required by SSSD's UCR template (sssd.conf
		// domain section) and by UMC's saml/sp.py which builds the cert path as
		// /etc/univention/ssl/<hostname>.<domainname>/cert.pem.
		fmt.Sprintf("hostname: %s", tenant.Name),
		fmt.Sprintf("domainname: %s", r.KernelDomain),
		// ldap/master must be a plain hostname (not a URL) — UCR templates build
		// ldap:// URIs themselves.
		fmt.Sprintf("ldap/master: %s", umcLDAPServerName),
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
		// the tenant effective domain so certs and redirects are correct.
		fmt.Sprintf("umc/saml/sp-server: %s", effectiveDomain),
		fmt.Sprintf("umc/saml/idp-server: %s", samlIDPExternal),
		fmt.Sprintf("umc/saml/idp-server-internal: %s", samlIDPInternal),
	}, umcApacheUCRLines()...), append(umcOIDCUCRLines(keycloakExternal, keycloakInternal, oidcClientID), append([]string{
		// UMC's Tornado HTTP server defaults to binding on 127.0.0.1. The
		// Kubernetes liveness/readiness probes connect to the pod IP, so UMC
		// must bind on all interfaces for the probes (and the Traefik proxy
		// pod) to reach it.
		"umc/http/interface: 0.0.0.0",
	}, smtpLines...)...)...), "\n") + "\n"

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      umcUCRConfigMapName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
			Annotations: map[string]string{
				reloaderMatchAnnotation: "true",
			},
		},
		Data: map[string]string{
			"base.conf":          baseForcedConf,
			"base-defaults.conf": umcUCRBaseDefaultsConf,
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
		if err := r.Patch(ctx, existing, patch); err != nil {
			return err
		}
	}
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	if existing.Annotations[reloaderMatchAnnotation] != "true" {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Annotations[reloaderMatchAnnotation] = "true"
		if err := r.Patch(ctx, existing, patch); err != nil {
			return err
		}
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
	if err := r.deleteUMCNubusLoginRedirects(ctx, tenant, tenantNamespaceName(tenant)); err != nil {
		return err
	}
	if err := r.deleteUMCLegacyGentianLogin(ctx, tenant, tenantNamespaceName(tenant)); err != nil {
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
		"podAnnotations": map[string]interface{}{
			reloaderAutoAnnotation: "true",
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
					// API paths only — static UMC shell and Nubus login UI are served
					// by umc-gateway (/univention/login/, /univention/management/, …).
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
			"enableLoginPath":  true,
			"certManager": map[string]interface{}{
				"enabled": false,
			},
			"tls": map[string]interface{}{
				"enabled":    true,
				"secretName": kernelWildcardTenantSecret,
			},
			"annotations": umcIngressAnnotations(effectiveDomain, kernelDomain),
		},
		"podAnnotations": map[string]interface{}{
			reloaderAutoAnnotation: "true",
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

func umcApacheUCRLines() []string {
	return []string{
		"apache2/autostart: yes",
		"apache2/documentroot: /var/www/",
		"apache2/loglevel: info",
		"apache2/startsite: univention/",
		"apache2/force_https/exclude/http_host/localhost: localhost",
	}
}

// umcOIDCUCRLines returns UCR keys required for /usr/share/univention-management-
// console/oidc/oidc.json generation (see kernel nubus-dev-stack-data-ums-ucr).
func umcOIDCUCRLines(keycloakExternal, keycloakInternal, oidcClientID string) []string {
	return []string{
		"umc/oidc/default-op: nubus",
		fmt.Sprintf("umc/oidc/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/issuer-internal: %s", keycloakInternal),
		fmt.Sprintf("umc/oidc/nubus/issuer: %s", keycloakExternal),
		fmt.Sprintf("umc/oidc/nubus/client-id: %s", oidcClientID),
		"umc/oidc/nubus/client-secret-file: /etc/oidc-rp-umc-server.secret",
		"umc/oidc/nubus/extra-parameter: kc_idp_hint",
		"umc/oidc/nubus/openid-certs: /usr/share/univention-management-console/oidc/nubus.jwks",
		"umc/oidc/nubus/openid-configuration: /usr/share/univention-management-console/oidc/nubus.json",
		"umc/oidc/rp/server: nubus",
	}
}

func umcGentianLoginName(tenantName string) string {
	return fmt.Sprintf("umc-%s-gentian-login", tenantName)
}

func umcRootRedirectName(tenantName string) string {
	return fmt.Sprintf("umc-%s-root-redirect", tenantName)
}

func umcLoginRedirectName(tenantName string) string {
	return fmt.Sprintf("umc-%s-login-redirect", tenantName)
}

func umcFrontendLabels(tenantName, instance string) map[string]string {
	return map[string]string{
		tenantLabel:                tenantName,
		managedByLabel:             managedByValue,
		umcFrontendComponentLabel:  umcFrontendComponentValue,
		"app.kubernetes.io/name":   "nubus-login-redirect",
		"app.kubernetes.io/instance": instance,
	}
}

func nubusTenantLoginURL(effectiveDomain string) string {
	u := url.URL{
		Scheme: "https",
		Host:   effectiveDomain,
		Path:   "/univention/login/",
	}
	q := u.Query()
	q.Set("location", nubusTenantLoginLocation)
	u.RawQuery = q.Encode()
	return u.String()
}

func (r *TenantReconciler) ensureUMCNubusLoginRedirects(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	gatewaySvc := umcGatewayReleaseName(tenant.Name)
	loginURL := nubusTenantLoginURL(effectiveDomain)
	redirectAnn := map[string]string{
		"nginx.ingress.kubernetes.io/permanent-redirect":   loginURL,
		"nginx.ingress.kubernetes.io/ssl-redirect":       "false",
		"nginx.ingress.kubernetes.io/force-ssl-redirect": "false",
	}

	rootName := umcRootRedirectName(tenant.Name)
	if err := r.ensureNginxRedirectIngress(ctx, nsName, effectiveDomain, rootName, tenant.Name, redirectAnn, gatewaySvc, []redirectPath{
		{path: "/", pathType: networkingv1.PathTypePrefix},
	}); err != nil {
		return fmt.Errorf("root redirect ingress: %w", err)
	}

	loginName := umcLoginRedirectName(tenant.Name)
	return r.ensureNginxRedirectIngress(ctx, nsName, effectiveDomain, loginName, tenant.Name, redirectAnn, gatewaySvc, []redirectPath{
		{path: "/login", pathType: networkingv1.PathTypePrefix},
	})
}

type redirectPath struct {
	path     string
	pathType networkingv1.PathType
}

func (r *TenantReconciler) ensureNginxRedirectIngress(
	ctx context.Context,
	nsName, effectiveDomain, name, tenantName string,
	annotations map[string]string,
	backendSvc string,
	paths []redirectPath,
) error {
	ingressClass := "public"
	labels := umcFrontendLabels(tenantName, name)
	httpPaths := make([]networkingv1.HTTPIngressPath, 0, len(paths))
	for _, p := range paths {
		pt := p.pathType
		httpPaths = append(httpPaths, networkingv1.HTTPIngressPath{
			Path:     p.path,
			PathType: &pt,
			Backend:  ingressBackend(backendSvc),
		})
	}
	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   nsName,
			Labels:      labels,
			Annotations: annotations,
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
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: httpPaths},
				},
			}},
		},
	}
	existing := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ingressSpecEqual(existing, desired) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		existing.Annotations = desired.Annotations
		existing.Labels = desired.Labels
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *TenantReconciler) deleteUMCNubusLoginRedirects(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	for _, name := range []string{umcRootRedirectName(tenant.Name), umcLoginRedirectName(tenant.Name)} {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}}
		if err := r.Delete(ctx, ing); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// deleteUMCLegacyGentianLogin removes the superseded gentian-ui portal-frontend
// stack (Deployment, Service, ConfigMap, Ingress) from clusters reconciled before
// Nubus /univention/login/ became the tenant entry point.
func (r *TenantReconciler) deleteUMCLegacyGentianLogin(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	name := umcGentianLoginName(tenant.Name)
	for _, obj := range []client.Object{
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-branding", Namespace: nsName}},
	} {
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	// Deployment type removed from imports; delete via unstructured if present.
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	deploy.SetName(name)
	deploy.SetNamespace(nsName)
	if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
		return err
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

func ingressSpecEqual(a, b *networkingv1.Ingress) bool {
	return equality.Semantic.DeepEqual(a.Spec, b.Spec) &&
		equality.Semantic.DeepEqual(a.Annotations, b.Annotations) &&
		equality.Semantic.DeepEqual(a.Labels, b.Labels)
}
