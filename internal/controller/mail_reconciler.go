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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"time"

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
	conditionMailReady = "MailReady"
	mailRequeueAfter   = 2 * time.Second
	dkimKeySize        = 2048

	// ConfigMaps in the kernel namespace that hold the shared mail infrastructure
	// tenant registry. The mail reconciler adds/removes entries as tenants are
	// provisioned or deleted.
	mailPostfixVirtualDomainsConfigMap = "mail-postfix-virtual-domains"
	mailDovecotDomainsConfigMap        = "mail-dovecot-domains"
	mailDovecotOIDCValuesConfigMap     = "dovecot-tenant-oidc-values"

	// mailSharedPostfixPort is the cluster-internal submission port for shared Postfix.
	mailSharedPostfixPort = "587"

	// smtpPasswordLength is the number of random bytes used to generate per-tenant
	// SMTP passwords, producing a 32-character base64url-encoded string.
	smtpPasswordLength = 24
)

// mailSharedPostfixHost returns the in-cluster Postfix submission hostname.
// Override with MAIL_SMTP_HOST; otherwise postfix-{stage}.{servicesNamespace}.
func mailSharedPostfixHost() string {
	if v := envOrDefault("MAIL_SMTP_HOST", ""); v != "" {
		return v
	}
	stage := envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
	return fmt.Sprintf("postfix-%s.%s.svc.cluster.local", stage, servicesNamespace)
}

// ensureMail provisions the mail stack for the tenant according to spec.mail.mode.
// It dispatches to one of four mode-specific handlers and sets the MailReady condition.
func (r *TenantReconciler) ensureMail(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	mode := gentianov1alpha1.MailModeSelfhosted
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" {
		mode = tenant.Spec.Mail.Mode
	}

	switch mode {
	case gentianov1alpha1.MailModeSelfhosted:
		done, err := r.ensureMailSelfhosted(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"Provisioning", "Waiting for Dovecot tenant OIDC client in Keycloak")
			return ctrl.Result{RequeueAfter: mailRequeueAfter}, nil
		}
		if err := r.seedPerAppMailSecrets(ctx, tenant); err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "SeedFailed", err.Error())
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"Selfhosted", "Tenant registered in shared Postfix and Dovecot infrastructure")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeExternal:
		// Guard first: the SmtpCredentialsSecret reference is mandatory for this mode.
		if tenant.Spec.Mail == nil || tenant.Spec.Mail.SmtpCredentialsSecret == "" {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"MissingConfig", "spec.mail.smtpCredentialsSecret is required for mode=external")
			return ctrl.Result{}, nil
		}
		done, err := r.ensureMailExternal(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if !done {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
				"Provisioning", "Waiting for SMTP credentials to be available")
			return ctrl.Result{RequeueAfter: mailRequeueAfter}, nil
		}
		if err := r.seedPerAppMailSecrets(ctx, tenant); err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "SeedFailed", err.Error())
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"External", "SMTP credentials have been propagated to the tenant namespace")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeTransportOnly:
		err := r.ensureMailTransportOnly(ctx, tenant)
		if err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
			return ctrl.Result{}, err
		}
		if err := r.seedPerAppMailSecrets(ctx, tenant); err != nil {
			r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse, "SeedFailed", err.Error())
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"TransportOnly", "Tenant registered in shared Postfix relay")
		return ctrl.Result{}, nil

	case gentianov1alpha1.MailModeDisabled:
		r.setCondition(tenant, conditionMailReady, metav1.ConditionTrue,
			"MailDisabled", "Mail stack is disabled for this tenant")
		return ctrl.Result{}, nil

	default:
		r.setCondition(tenant, conditionMailReady, metav1.ConditionFalse,
			"UnknownMode", fmt.Sprintf("unknown mail mode %q", mode))
		return ctrl.Result{}, nil
	}
}

// ensureMailSelfhosted provisions full mail capability for the tenant by registering
// tenant-scoped configuration within the shared kernel mail infrastructure:
//  1. A DKIM RSA-2048 private key Secret in the kernel namespace (created once; never
//     auto-rotated). The public key and suggested DNS records are written to TenantStatus.
//  2. An entry in the shared Postfix virtual-domains ConfigMap (kernel namespace).
//  3. A per-tenant SMTP credentials Secret in the tenant namespace, allowing the
//     tenant's apps to authenticate to the shared Postfix submission endpoint.
//  4. An entry in the shared Dovecot domains ConfigMap (kernel namespace), enabling
//     IMAP storage at the tenant-scoped path /var/mail/{domain}/{user}.
func (r *TenantReconciler) ensureMailSelfhosted(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	// 1. DKIM key Secret in kernel namespace (idempotent — generate once, never rotate automatically).
	pubKey, err := r.ensureDKIMSecret(ctx, tenant)
	if err != nil {
		return false, fmt.Errorf("ensure DKIM key Secret: %w", err)
	}
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if tenant.Status.Mail == nil {
		tenant.Status.Mail = &gentianov1alpha1.TenantMailStatus{}
	}
	if pubKey != "" {
		tenant.Status.Mail.DKIMPublicKey = pubKey
	}
	tenant.Status.Mail.SPFRecord = "v=spf1 mx ~all"
	tenant.Status.Mail.DMARCRecord = fmt.Sprintf("v=DMARC1; p=none; rua=mailto:dmarc@%s", domain)

	// 2. Register the tenant domain in the shared Postfix virtual-domains ConfigMap.
	if err := r.ensurePostfixVirtualDomain(ctx, tenant); err != nil {
		return false, fmt.Errorf("register Postfix virtual domain: %w", err)
	}

	// 3. Provision per-tenant SMTP credentials in the tenant namespace.
	if err := r.ensureSmtpCredentialsSecret(ctx, tenant); err != nil {
		return false, fmt.Errorf("ensure SMTP credentials Secret: %w", err)
	}

	// 4. Register the tenant domain in the shared Dovecot domains ConfigMap.
	if err := r.ensureDovecotDomainConfig(ctx, tenant); err != nil {
		return false, fmt.Errorf("register Dovecot domain config: %w", err)
	}

	// 5. Ensure gentian-dovecot exists in the tenant realm for IMAP XOAUTH2 introspection.
	if ready, err := r.ensureDovecotTenantOIDCClientJob(ctx, tenant); err != nil {
		return false, fmt.Errorf("ensure Dovecot tenant OIDC client: %w", err)
	} else if !ready {
		return false, nil
	}

	// 6. Point shared Dovecot token introspection at the tenant realm that issues OX tokens.
	if err := r.ensureDovecotTenantOIDCIntrospection(ctx, tenant); err != nil {
		return false, fmt.Errorf("configure Dovecot tenant OIDC introspection: %w", err)
	}

	return true, nil
}

// ensureMailExternal propagates SMTP relay credentials from the kernel namespace into
// the tenant namespace as a plain Kubernetes Secret. The source secret is identified by
// spec.mail.smtpCredentialsSecret (name in the kernel namespace).
//
// The caller (ensureMail) is responsible for validating that SmtpCredentialsSecret is
// non-empty before invoking this function.
//
// Returns false (with a requeue) while the source secret is not yet present.
func (r *TenantReconciler) ensureMailExternal(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	srcName := tenant.Spec.Mail.SmtpCredentialsSecret

	// Fetch the source Secret from the kernel namespace.
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: srcName, Namespace: kernelNamespace}, src); err != nil {
		if errors.IsNotFound(err) {
			return false, nil // source not yet available — requeue
		}
		return false, fmt.Errorf("get SMTP credentials secret %s/%s: %w", kernelNamespace, srcName, err)
	}

	// Create the Secret in the tenant namespace if it does not already exist.
	dstName := smtpCredentialsSecretName(tenant.Name)
	dst := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: dstName, Namespace: nsName}, dst)
	if errors.IsNotFound(err) {
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dstName,
				Namespace: nsName,
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Data: src.Data,
			Type: src.Type,
		}
		if createErr := r.Create(ctx, desired); createErr != nil {
			return false, createErr
		}
		return true, nil // Secret was just created; credentials are now available.
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ensureMailTransportOnly registers the tenant domain in the shared Postfix virtual-
// domains ConfigMap and provisions SMTP credentials in the tenant namespace.
// No Dovecot IMAP storage is configured — outbound relay only.
func (r *TenantReconciler) ensureMailTransportOnly(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	// Register domain in shared Postfix for outbound relay.
	if err := r.ensurePostfixVirtualDomain(ctx, tenant); err != nil {
		return fmt.Errorf("register Postfix virtual domain: %w", err)
	}

	// Provision SMTP credentials so apps can authenticate to the shared relay.
	if err := r.ensureSmtpCredentialsSecret(ctx, tenant); err != nil {
		return fmt.Errorf("ensure SMTP credentials Secret: %w", err)
	}

	return nil
}

// ensurePostfixVirtualDomain adds the tenant's mail domain to the shared Postfix
// virtual-domains ConfigMap in the kernel namespace. The ConfigMap is created if it
// does not yet exist. This is idempotent — existing entries are not modified.
func (r *TenantReconciler) ensurePostfixVirtualDomain(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: mailPostfixVirtualDomainsConfigMap, Namespace: kernelNamespace}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mailPostfixVirtualDomainsConfigMap,
				Namespace: kernelNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: map[string]string{tenant.Name: domain},
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}

	// Entry already present — nothing to do.
	if cm.Data[tenant.Name] == domain {
		return nil
	}

	// Add or update the entry for this tenant.
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[tenant.Name] = domain
	return r.Update(ctx, cm)
}

// ensureDovecotDomainConfig adds the tenant's mail domain to the shared Dovecot
// domains ConfigMap in the kernel namespace. The ConfigMap is created if it does not
// yet exist. Dovecot uses this to map each domain to a tenant-scoped mailbox path
// (/var/mail/{domain}/{user}).
func (r *TenantReconciler) ensureDovecotDomainConfig(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	domain := mailDomain(tenant, r.KernelDomain, r.TenancyMode)
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: mailDovecotDomainsConfigMap, Namespace: kernelNamespace}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mailDovecotDomainsConfigMap,
				Namespace: kernelNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: map[string]string{tenant.Name: domain},
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}

	if cm.Data[tenant.Name] == domain {
		return nil
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[tenant.Name] = domain
	return r.Update(ctx, cm)
}

// ensureDovecotTenantOIDCIntrospection patches the shared Dovecot Helm values
// ConfigMap so IMAP XOAUTH2 introspection targets the tenant Keycloak realm.
// OX App Suite issues access tokens in the tenant realm; kernel-realm introspection
// rejects them and the App Suite UI fails to load its configuration namespace.
func (r *TenantReconciler) ensureDovecotTenantOIDCIntrospection(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	realmName := keycloakRealmName(tenant)
	ns := servicesNamespace
	path := fmt.Sprintf("/realms/%s/protocol/openid-connect/token/introspect", realmName)
	values := fmt.Sprintf(`dovecot:
  oidc:
    introspectionPath: %q
`, path)

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: mailDovecotOIDCValuesConfigMap, Namespace: ns}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mailDovecotOIDCValuesConfigMap,
				Namespace: ns,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: map[string]string{"values.yaml": values},
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	if cm.Data["values.yaml"] == values {
		return nil
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["values.yaml"] = values
	return r.Update(ctx, cm)
}

// ensureSmtpCredentialsSecret creates a per-tenant SMTP credentials Secret in the
// tenant namespace. Apps use these to authenticate to the shared Postfix submission
// endpoint (mailSharedPostfixHost:587). The password is generated on first creation and
// never rotated automatically; the username is smtp-{tenant-name}.
func (r *TenantReconciler) ensureSmtpCredentialsSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	secretName := smtpCredentialsSecretName(tenant.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existing)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	// Generate a random SMTP password.
	passBytes := make([]byte, smtpPasswordLength)
	if _, randErr := rand.Read(passBytes); randErr != nil {
		return fmt.Errorf("generate SMTP password for tenant %s: %w", tenant.Name, randErr)
	}
	password := base64.RawURLEncoding.EncodeToString(passBytes)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		StringData: map[string]string{
			"host":     mailSharedPostfixHost(),
			"port":     mailSharedPostfixPort,
			"username": fmt.Sprintf("smtp-%s", tenant.Name),
			"password": password,
		},
	}
	return r.Create(ctx, secret)
}

// seedPerAppMailSecrets writes each app's SMTP/IMAP KV record into OpenBao so
// the app reconciler can inject the credentials as Helm values.
// The SMTP password is *copied* from the per-tenant SMTP credentials Secret —
// it is shared across all apps of a tenant since they authenticate to the
// same Postfix submission endpoint with one user. IMAP gets only host/port
// (per-user credentials come from Keycloak/OIDC at runtime).
//
// No-op when the Seeder is nil (envtest / staged rollout). In MailModeDisabled
// the caller never invokes this function.
func (r *TenantReconciler) seedPerAppMailSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if r.Seeder == nil {
		return nil
	}

	// Collect the set of apps that need SMTP and/or IMAP.
	type need struct{ smtp, imap bool }
	needs := map[string]need{}
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Mail == nil {
			continue
		}
		needs[app.Profile] = need{
			smtp: profile.Spec.KernelRequirements.Mail.SMTP != nil,
			imap: profile.Spec.KernelRequirements.Mail.IMAP != nil,
		}
	}
	if len(needs) == 0 {
		return nil
	}

	// Pull the per-tenant SMTP secret so we can copy the same credentials into
	// each app's OpenBao path. Under MailModeExternal this Secret was just
	// created from the admin-provided source; under Selfhosted/TransportOnly it
	// was created by ensureSmtpCredentialsSecret.
	nsName := tenantNamespaceName(tenant)
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: smtpCredentialsSecretName(tenant.Name), Namespace: nsName}, src); err != nil {
		return fmt.Errorf("read tenant SMTP secret: %w", err)
	}
	smtp := secrets.SMTPCreds{
		Host:     string(src.Data["host"]),
		Port:     string(src.Data["port"]),
		User:     string(src.Data["username"]),
		Password: string(src.Data["password"]),
	}

	for appName, n := range needs {
		// Seed SMTP credentials into OpenBao.
		effectiveTenant := tenant.Name
		if n.smtp {
			if _, err := r.Seeder.SeedSMTP(ctx, effectiveTenant, appName, smtp); err != nil {
				return fmt.Errorf("seed smtp for %s: %w", appName, err)
			}
		}
		if n.imap {
			if err := r.Seeder.SeedIMAP(ctx, effectiveTenant, appName, secrets.IMAPCreds{
				Host: "dovecot.platform-kernel.svc.cluster.local",
				Port: "143",
			}); err != nil {
				return fmt.Errorf("seed imap for %s: %w", appName, err)
			}
		}
	}
	return nil
}

// deleteMail removes the tenant's registration from the shared mail infrastructure.
//
// The tenant domain is always removed from the shared Postfix and Dovecot ConfigMaps so
// that those instances stop accepting mail for the domain. Under DeletionPolicy=Delete
// the DKIM key Secret (kernel namespace) and SMTP credentials Secret (tenant namespace)
// are also removed; under Retain they are preserved.
func (r *TenantReconciler) deleteMail(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	mode := gentianov1alpha1.MailModeSelfhosted
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" {
		mode = tenant.Spec.Mail.Mode
	}

	// Remove domain entries from shared ConfigMaps (always — these affect live routing).
	if mode == gentianov1alpha1.MailModeSelfhosted || mode == gentianov1alpha1.MailModeTransportOnly {
		if err := r.removeFromMailConfigMap(ctx, mailPostfixVirtualDomainsConfigMap, tenant.Name); err != nil {
			return fmt.Errorf("remove Postfix virtual domain for tenant %s: %w", tenant.Name, err)
		}
	}
	if mode == gentianov1alpha1.MailModeSelfhosted {
		if err := r.removeFromMailConfigMap(ctx, mailDovecotDomainsConfigMap, tenant.Name); err != nil {
			return fmt.Errorf("remove Dovecot domain config for tenant %s: %w", tenant.Name, err)
		}
	}

	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	// DeletionPolicy=Delete: also remove Secrets.
	nsName := tenantNamespaceName(tenant)
	if mode == gentianov1alpha1.MailModeSelfhosted || mode == gentianov1alpha1.MailModeTransportOnly {
		// SMTP credentials Secret in tenant namespace.
		smtpSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: smtpCredentialsSecretName(tenant.Name), Namespace: nsName,
		}}
		if err := r.Delete(ctx, smtpSec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete SMTP credentials secret for tenant %s: %w", tenant.Name, err)
		}
	}
	if mode == gentianov1alpha1.MailModeSelfhosted {
		// DKIM key Secret in kernel namespace.
		dkimSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: dkimSecretName(tenant.Name), Namespace: kernelNamespace,
		}}
		if err := r.Delete(ctx, dkimSec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete DKIM secret for tenant %s: %w", tenant.Name, err)
		}
	}
	if mode == gentianov1alpha1.MailModeExternal {
		smtpSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: smtpCredentialsSecretName(tenant.Name), Namespace: nsName,
		}}
		if err := r.Delete(ctx, smtpSec); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete SMTP credentials secret for tenant %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// removeFromMailConfigMap removes the tenant key from a shared mail ConfigMap.
// It is a no-op when the ConfigMap or key does not exist.
func (r *TenantReconciler) removeFromMailConfigMap(ctx context.Context, cmName, tenantName string) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: kernelNamespace}, cm)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := cm.Data[tenantName]; !ok {
		return nil // key not present — nothing to do
	}
	delete(cm.Data, tenantName)
	return r.Update(ctx, cm)
}

// ensureDKIMSecret creates a DKIM RSA-2048 private key Secret in the kernel namespace
// if one does not already exist. The kernel namespace is used so that the shared Rspamd
// instance can read the key directly.
//
// Returns the base64-encoded PKIX DER public key suitable for publishing in a DKIM TXT
// record, or "" when the secret was just created (will be derived on the next pass).
func (r *TenantReconciler) ensureDKIMSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant) (string, error) {
	secretName := dkimSecretName(tenant.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: kernelNamespace}, existing)
	if err == nil {
		// Secret already exists — derive the public key from the stored private key.
		privPEM, ok := existing.Data["tls.key"]
		if !ok {
			return "", fmt.Errorf("DKIM secret %s/%s is missing key tls.key", kernelNamespace, secretName)
		}
		block, _ := pem.Decode(privPEM)
		if block == nil {
			return "", fmt.Errorf("DKIM secret %s/%s: tls.key is not valid PEM", kernelNamespace, secretName)
		}
		priv, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if parseErr != nil {
			return "", fmt.Errorf("parse DKIM private key in %s/%s: %w", kernelNamespace, secretName, parseErr)
		}
		pubDER, marshalErr := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if marshalErr != nil {
			return "", fmt.Errorf("marshal DKIM public key for %s/%s: %w", kernelNamespace, secretName, marshalErr)
		}
		return base64.StdEncoding.EncodeToString(pubDER), nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	// Generate a fresh RSA-2048 key pair.
	priv, err := rsa.GenerateKey(rand.Reader, dkimKeySize)
	if err != nil {
		return "", fmt.Errorf("generate DKIM RSA key for tenant %s: %w", tenant.Name, err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal DKIM public key for tenant %s: %w", tenant.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Data: map[string][]byte{
			"tls.key": privPEM,
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("create DKIM secret %s/%s: %w", kernelNamespace, secretName, err)
	}
	return base64.StdEncoding.EncodeToString(pubDER), nil
}

// --- Name helpers ------------------------------------------------------------

// mailDomain returns the effective mail domain for a tenant: spec.mail.domain
// if set, otherwise the tenant's effective ingress domain (vanity or
// <tenant>.<kernel_domain> fallback). See architecture §2.5.
func mailDomain(tenant *gentianov1alpha1.Tenant, kernelDomain, tenancyMode string) string {
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Domain != "" {
		return tenant.Spec.Mail.Domain
	}
	return tenant.EffectiveDomain(kernelDomain, tenancyMode)
}

func dkimSecretName(tenantName string) string {
	return fmt.Sprintf("dkim-%s", tenantName)
}

func smtpCredentialsSecretName(tenantName string) string {
	return fmt.Sprintf("smtp-credentials-%s", tenantName)
}
