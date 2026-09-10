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
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// dovecotRealmAuthSecret holds Dovecot's XOAUTH2 configuration, two files per
	// Keycloak realm whose users have mailboxes. A Secret rather than a ConfigMap
	// because the files carry the introspection client secret.
	//
	// It replaces dovecot-tenant-oidc-values, which held a single values.yaml key
	// rewritten by every tenant reconcile — so the last tenant to reconcile owned
	// the only introspection target and every other tenant's IMAP logins failed.
	dovecotRealmAuthSecret = "dovecot-realm-auth"

	// Where the Dovecot Pod mounts the above. Referenced from the generated passdb
	// args, so it has to agree with the volumeMount in
	// kernel/services/dovecot/manifests/templates/deployment.yaml.
	dovecotRealmAuthMountPath = "/etc/dovecot/realms"

	// Stamped on the Dovecot Pod template to restart it when the realm set changes.
	// Excluded from the Argo CD diff by the 09-infra-helm ApplicationSet.
	dovecotRealmAuthHashAnnotation = "gentianos.io/realm-auth-hash"

	// The two texthash: files kernel Postfix mounts, in the mail namespace. Both
	// are derived from the registry above: the registry says which tenant domains
	// exist, these say which of them Postfix accepts mail for and where each
	// address is delivered. A domain present in one but not the other is either
	// bounced or refused outright, so they are always written together.
	postfixVirtualMailboxMapsConfigMap = "postfix-kernel-virtual-mailbox-maps"
	postfixVirtualMailboxDomainsKey    = "virtual_mailbox_domains"
	postfixSenderAccessKey             = "sender_access"
	// The domain list OpenDKIM signs for, space-separated, for the image's
	// ALLOWED_SENDER_DOMAINS. Unlike the maps beside it this is read once at
	// start, not per lookup, so a new tenant signs only after Postfix restarts.
	postfixAllowedSenderDomainsKey = "allowed_sender_domains"
	postfixVirtualMailboxMapsKey   = "virtual_mailbox_maps"

	// OpenDKIM decides which key signs which domain from these two tables. The
	// operator generates a key per tenant already; without the tables OpenDKIM
	// never learns of it, so tenant mail leaves unsigned while its DNS record
	// sits published and unused.
	postfixDKIMSecret = "postfix-dkim-tenants"
	// The kernel domain's own DKIM key, held on the same terms as a tenant's.
	kernelDKIMSecret        = "dkim-kernel"
	postfixDKIMKeyTableKey  = "KeyTable"
	postfixDKIMSignTableKey = "SigningTable"
	// The selector the Postfix image uses for its own key. Tenants reuse it so
	// one published record shape covers every domain.
	postfixDKIMSelector = "mail"

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

// dovecotDeployed reports whether this cluster runs the IMAP server that the
// Dovecot steps below configure.
//
// The operator configured it unconditionally: a Keycloak client per realm, a
// Job per reconcile, a realm-auth Secret and a domains ConfigMap — all for an
// IMAP server that, with mail.serviceMode external, is not deployed at all.
// Nothing failed, which is why it went unnoticed; it simply provisioned into
// a void and left Keycloak clients behind for a service that never existed.
//
// Empty means external, the safer default: configuring an absent Dovecot is
// silent waste, while skipping a present one breaks IMAP authentication
// visibly and is fixed by setting the value.
//
// Read from the claim through gentian-cluster-config rather than from the
// MailServiceMode field, which is a Helm value and was answering external on a
// cluster whose claim said kernel — so Dovecot ran, unprovisioned, and the
// ApplicationSet that would have managed it was never rendered. The field
// remains as the fallback for a cluster whose ConfigMap cannot answer yet.
func (r *TenantReconciler) dovecotDeployed(ctx context.Context) bool {
	return clusterMailServiceMode(ctx, r.Client, r.MailServiceMode) == "kernel"
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
	// SPF names the address mail LEAVES from. "mx" names the MX host, which is
	// the inbound load balancer and never sends anything — so on any cluster
	// whose inbound and outbound addresses differ, that record fails by
	// construction while looking plausible.
	//
	// a:<egressHost> rather than an ip4: literal, so the record follows the
	// egress A record instead of having to be edited in two places whenever the
	// address changes; the one that gets forgotten fails closed and silently.
	if egress := clusterMailEgressHost(ctx, r.Client, envOrDefault("MAIL_EGRESS_HOST", "")); egress != "" {
		tenant.Status.Mail.SPFRecord = "v=spf1 a:" + egress + " -all"
	} else {
		// No dedicated egress: the cluster sends from a shared address or relays
		// through a smarthost, and mx is the best guess available here.
		tenant.Status.Mail.SPFRecord = "v=spf1 mx ~all"
	}
	tenant.Status.Mail.DMARCRecord = fmt.Sprintf("v=DMARC1; p=none; rua=mailto:dmarc@%s", domain)

	// 2. Register the tenant domain in the shared Postfix virtual-domains ConfigMap.
	if err := r.ensurePostfixVirtualDomain(ctx, tenant); err != nil {
		return false, fmt.Errorf("register Postfix virtual domain: %w", err)
	}

	// 3. Provision per-tenant SMTP credentials in the tenant namespace.
	if err := r.ensureSmtpCredentialsSecret(ctx, tenant); err != nil {
		return false, fmt.Errorf("ensure SMTP credentials Secret: %w", err)
	}

	// 3b. A mail password per user, per client app, so a webmail client can
	// reach IMAP without anyone holding a password an OIDC login never issued.
	//
	// Logged rather than returned: Keycloak being briefly unreachable should not
	// fail the whole tenant reconcile, and a user without a password yet sees a
	// mail client that cannot sign in — not a tenant that fails to provision.
	if err := r.syncMailAppPasswords(ctx, tenant); err != nil {
		log.FromContext(ctx).Error(err, "sync mail app passwords", "tenant", tenant.Name)
	}

	// 3c. The tenant's mail DNS — MX, SPF, DKIM, DMARC — for external-dns to
	// reconcile into the zone. Logged rather than returned: a cluster without
	// external-dns keeps these records manual, which is a missing convenience
	// rather than a broken tenant.
	if err := r.syncTenantMailDNS(ctx, tenant); err != nil {
		log.FromContext(ctx).Error(err, "publish mail DNS records", "tenant", tenant.Name)
	}

	// 4-6. Dovecot, only where Dovecot exists.
	//
	// Steps 4 to 6 register the tenant's domain, create gentian-dovecot in its
	// realm and add that realm to Dovecot's XOAUTH2 configuration. All three
	// configure an IMAP server, and a cluster in external mail mode does not
	// run one — its mailboxes are at the provider. Running them anyway left a
	// Keycloak client per realm and a Job per reconcile behind, addressed to
	// nothing.
	if r.dovecotDeployed(ctx) {
		// 4. Register the tenant domain in the shared Dovecot domains ConfigMap.
		if err := r.ensureDovecotDomainConfig(ctx, tenant); err != nil {
			return false, fmt.Errorf("register Dovecot domain config: %w", err)
		}

		// 5. Wait for gentian-dovecot in the tenant realm, which IMAP XOAUTH2
		//    introspection authenticates as. tenant-default composes the client;
		//    this only waits for it, because steps 6 and 7 configure Dovecot to
		//    introspect with it and would otherwise point at a client that does
		//    not exist yet.
		if ready, err := r.dovecotTenantClientReady(ctx, tenant); err != nil {
			return false, fmt.Errorf("check Dovecot tenant OIDC client: %w", err)
		} else if !ready {
			return false, nil
		}

		// 6. Add this tenant's realm to Dovecot's XOAUTH2 configuration. Additive, so a
		//    second tenant does not displace the first — see ensureDovecotRealmAuth.
		if err := r.ensureDovecotRealmAuth(ctx, keycloakRealmName(tenant)); err != nil {
			return false, fmt.Errorf("configure Dovecot realm auth: %w", err)
		}
	}

	// 7. Restart Dovecot if the realm set changed. A no-op when it did not, so this
	//    does not bounce mail on every reconcile.
	if err := r.ensureDovecotAuthReload(ctx); err != nil {
		return false, fmt.Errorf("reload Dovecot auth config: %w", err)
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
		if err := r.Create(ctx, cm); err != nil {
			return err
		}
		return r.syncPostfixVirtualMailboxMaps(ctx)
	}
	if err != nil {
		return err
	}

	// Add or update the entry for this tenant.
	if cm.Data[tenant.Name] != domain {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[tenant.Name] = domain
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
	}

	// Sync unconditionally rather than only when the registry changed: the map is
	// a separate object that can be absent or stale for reasons this reconcile
	// cannot see, and the sync is a no-op write when it already matches.
	if err := r.syncPostfixVirtualMailboxMaps(ctx); err != nil {
		return err
	}
	// Signing tables follow the same registry. A failure here must not fail the
	// address maps: unsigned mail is deliverable, mail refused for an unknown
	// domain is not.
	if err := r.syncPostfixDKIMTables(ctx); err != nil {
		log.FromContext(ctx).Error(err, "sync OpenDKIM tables; tenant mail will send unsigned")
	}
	return nil
}

// syncPostfixVirtualMailboxMaps rebuilds the two texthash: files kernel Postfix
// mounts from the tenant domain registry.
//
// Registering a domain and routing mail to it are otherwise two separate steps
// with only one owner. The installer seeds these files from whatever tenants
// exist at install time; a tenant provisioned afterwards registers its domain,
// so without this the files would not name it and mail to that tenant —
// including the admin address the platform derives for it — is refused. The
// registry's owner owns what is derived from it.
//
// Postfix mounts the ConfigMap as a directory and reads both files at lookup
// time, so a rewrite here takes effect without restarting Postfix.
//
// Rewritten whole rather than appended to: the files are a function of the
// registry, and a partial update is how the two drift apart.
func (r *TenantReconciler) syncPostfixVirtualMailboxMaps(ctx context.Context) error {
	registry := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: mailPostfixVirtualDomainsConfigMap, Namespace: kernelNamespace,
	}, registry); client.IgnoreNotFound(err) != nil {
		return err
	}
	// An absent registry is an empty one, and still writes both files. Postfix
	// treats a map file it cannot open as a lookup failure, not as an empty
	// result, so "no tenant domains" has to be spelled out rather than left to
	// a missing file.

	// Sorted, so an unchanged registry yields identical files and this does not
	// rewrite the object on every reconcile.
	names := make([]string, 0, len(registry.Data))
	for name := range registry.Data {
		names = append(names, name)
	}
	sort.Strings(names)

	// Deduplicated by domain, not by tenant. The registry is keyed by tenant and
	// two tenants can name the same mail domain — the kernel entry and a tenant
	// that inherited the kernel domain from a defaults component, for instance —
	// which emitted the same texthash line twice.
	var domainsFile, mapsFile strings.Builder
	domainList := make([]string, 0, len(names))
	emitted := make(map[string]bool, len(names))
	for _, name := range names {
		d := registry.Data[name]
		if d == "" || emitted[d] {
			continue
		}
		emitted[d] = true
		domainList = append(domainList, d)
		// "OK" is only a non-empty lookup result; Postfix reads the presence of
		// the key, not the value.
		fmt.Fprintf(&domainsFile, "%s OK\n", d)
		// Catch-all: every address at the domain is delivered into the domain's
		// Dovecot mailbox directory.
		fmt.Fprintf(&mapsFile, "@%s %s/\n", d, d)
	}
	desiredDomains, desiredMaps := domainsFile.String(), mapsFile.String()

	// The same domains, space-separated, for the image's ALLOWED_SENDER_DOMAINS.
	//
	// That variable is what makes OpenDKIM build a KeyTable entry for a domain,
	// so a domain missing here leaves mail unsigned however correct its key and
	// DNS record are. The kernel domain is prepended when the registry has not
	// listed it, because Postfix refuses to start with an empty list and an
	// empty registry would otherwise produce one.
	if r.KernelDomain != "" && !emitted[r.KernelDomain] {
		domainList = append([]string{r.KernelDomain}, domainList...)
	}
	desiredAllowed := strings.Join(domainList, " ")
	// The same "<domain> OK" lines drive check_sender_access, which decides who
	// may SEND. Without it, ALLOWED_SENDER_DOMAINS carries the kernel domain
	// alone, so a tenant user could receive mail but every message they sent was
	// refused with "Sender address rejected: Access denied" — outbound silently
	// limited to the kernel domain while inbound tracked every tenant.
	//
	// One derivation for both directions: a domain that may receive may send.
	desired := map[string]string{
		postfixVirtualMailboxDomainsKey: desiredDomains,
		postfixVirtualMailboxMapsKey:    desiredMaps,
		postfixSenderAccessKey:          desiredDomains,
		postfixAllowedSenderDomainsKey:  desiredAllowed,
	}

	maps := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name: postfixVirtualMailboxMapsConfigMap, Namespace: servicesNamespace,
	}, maps)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      postfixVirtualMailboxMapsConfigMap,
				Namespace: servicesNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: desired,
		})
	}
	if err != nil {
		return err
	}
	if maps.Data[postfixVirtualMailboxDomainsKey] == desiredDomains &&
		maps.Data[postfixVirtualMailboxMapsKey] == desiredMaps &&
		maps.Data[postfixSenderAccessKey] == desiredDomains &&
		maps.Data[postfixAllowedSenderDomainsKey] == desiredAllowed {
		return nil
	}
	if maps.Data == nil {
		maps.Data = make(map[string]string)
	}
	maps.Data[postfixVirtualMailboxDomainsKey] = desiredDomains
	maps.Data[postfixVirtualMailboxMapsKey] = desiredMaps
	maps.Data[postfixSenderAccessKey] = desiredDomains
	maps.Data[postfixAllowedSenderDomainsKey] = desiredAllowed
	return r.Update(ctx, maps)
}

// syncPostfixDKIMTables publishes the OpenDKIM KeyTable and SigningTable, plus
// the private keys they reference, as one Secret for Postfix to mount.
//
// The keys themselves are not created here — ensureDKIMSecret already generates
// one per tenant and never rotates it, because a rotated key silently stops
// matching the DNS record an operator published. This only tells OpenDKIM they
// exist.
//
// The kernel domain's own key is left to the image, which generates it at start
// from ALLOWED_SENDER_DOMAINS and writes it under /etc/opendkim/keys. Its
// KeyTable line is emitted here too, pointing at that path, because mounting a
// table over the image's own replaces the whole file — so a table that named
// only tenants would silently stop the kernel domain signing.
func (r *TenantReconciler) syncPostfixDKIMTables(ctx context.Context) error {
	kernelNamespace := defaultServicesNamespace()

	registry := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: mailPostfixVirtualDomainsConfigMap, Namespace: kernelNamespace,
	}, registry); client.IgnoreNotFound(err) != nil {
		return err
	}

	names := make([]string, 0, len(registry.Data))
	for name := range registry.Data {
		names = append(names, name)
	}
	sort.Strings(names)

	var keyTable, signTable strings.Builder
	data := map[string][]byte{}

	// The kernel domain first, on the same terms as a tenant: a key this
	// operator owns, seeded into Postfix and published from the same value.
	//
	// It used to be left to the image, which regenerates its key on every
	// restart unless the key directory is persistent. The record published for
	// a previous key stayed in DNS, so kernel-domain mail was signed with a key
	// no verifier could match — a dkim=fail that reads as a broken setup rather
	// than as the absent record it effectively was.
	//
	// Failure here is logged, not returned: unsigned mail is deliverable, and a
	// tenant reconcile must not fail over the kernel domain's signing key.
	if r.KernelDomain != "" {
		pub, priv, err := r.ensureDKIMKeyPair(ctx, kernelDKIMSecret, map[string]string{
			managedByLabel: managedByValue,
		})
		switch {
		case err != nil:
			log.FromContext(ctx).Error(err, "ensure kernel DKIM key; kernel-domain mail will send unsigned")
		default:
			data[r.KernelDomain+".private"] = priv
			fmt.Fprintf(&keyTable, "%s._domainkey.%s %s:%s:/etc/opendkim/keys/%s.private\n",
				postfixDKIMSelector, r.KernelDomain, r.KernelDomain, postfixDKIMSelector, r.KernelDomain)
			fmt.Fprintf(&signTable, "*@%s %s._domainkey.%s\n",
				r.KernelDomain, postfixDKIMSelector, r.KernelDomain)
			if dnsErr := r.syncKernelMailDNS(ctx, pub); dnsErr != nil {
				log.FromContext(ctx).Error(dnsErr, "publish kernel DKIM record")
			}
			// Alongside the records, because the label and the egress A record
			// are derived from the same node and must not disagree about which
			// node that is. Logged rather than returned: failing to label a node
			// should not fail a tenant reconcile, and the consequence is only
			// that Postfix may schedule where it already schedules today.
			if labelErr := r.syncMailEgressNodeLabel(ctx); labelErr != nil {
				log.FromContext(ctx).Error(labelErr, "label the mail egress node")
			}
		}
	}

	emitted := map[string]bool{r.KernelDomain: true}
	for _, name := range names {
		domain := registry.Data[name]
		if domain == "" || emitted[domain] {
			continue
		}

		// The key must exist before the domain is named in the table: OpenDKIM
		// refuses to load a table whose key file is missing, which would stop it
		// signing for every domain rather than just this one.
		keySecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: "dkim-" + name, Namespace: kernelNamespace,
		}, keySecret); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		priv, ok := keySecret.Data["tls.key"]
		if !ok || len(priv) == 0 {
			continue
		}

		emitted[domain] = true
		data[domain+".private"] = priv
		fmt.Fprintf(&keyTable, "%s._domainkey.%s %s:%s:/etc/opendkim/tenant-keys/%s.private\n",
			postfixDKIMSelector, domain, domain, postfixDKIMSelector, domain)
		fmt.Fprintf(&signTable, "*@%s %s._domainkey.%s\n",
			domain, postfixDKIMSelector, domain)
	}

	data[postfixDKIMKeyTableKey] = []byte(keyTable.String())
	data[postfixDKIMSignTableKey] = []byte(signTable.String())

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: postfixDKIMSecret, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      postfixDKIMSecret,
				Namespace: kernelNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: data,
		})
	}
	if err != nil {
		return err
	}
	same := len(existing.Data) == len(data)
	if same {
		for k, v := range data {
			if string(existing.Data[k]) != string(v) {
				same = false
				break
			}
		}
	}
	if same {
		return nil
	}
	existing.Data = data
	return r.Update(ctx, existing)
}

// postfixMapsBootstrap re-derives the Postfix inbound maps once, when the
// operator starts.
//
// Tenant events are otherwise the only trigger, which leaves two states
// uncovered: a cluster with no tenants, and one whose ConfigMap was written
// before Postfix began reading a second file from it. Neither is quiet —
// Postfix fails every recipient lookup against a map file that is not there, so
// mail the cluster used to relay stops as well.
//
// Registered by SetupWithManager, so it cannot be forgotten when the operator
// gains another entry point.
type postfixMapsBootstrap struct{ reconciler *TenantReconciler }

// Start runs after the manager's caches have synced.
//
// A failure is logged rather than returned: an unwritable ConfigMap is a mail
// fault, and taking the whole operator down for it would stop every other
// reconciler from making progress. The next tenant event retries.
func (b postfixMapsBootstrap) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)
	// The kernel side first, so the maps derived below already include the kernel
	// domain on a cluster that has no tenants.
	if err := b.reconciler.ensureKernelMail(ctx); err != nil {
		logger.Error(err, "provisioning kernel-realm mail at startup")
	}
	if err := b.reconciler.syncPostfixVirtualMailboxMaps(ctx); err != nil {
		logger.Error(err, "deriving Postfix inbound maps at startup")
	}
	// The DKIM tables Secret too, and for a harder reason than the maps: the
	// Postfix chart mounts postfix-dkim-tenants non-optional (a subPath over
	// a missing key would break the container in a worse way than refusing to
	// start), so on a cluster with no tenants the pod cannot even begin its
	// init until this Secret exists — and tenant events, its only other
	// trigger, never come. Empty tables are the correct content then: OpenDKIM
	// loads them fine and the kernel domain's own key is ensured inside.
	if err := b.reconciler.syncPostfixDKIMTables(ctx); err != nil {
		logger.Error(err, "deriving Postfix DKIM tables at startup")
	}
	return nil
}

// kernelMailRegistryKey is the entry in the tenant domain registry that holds the
// KERNEL domain rather than a tenant's.
//
// The underscore is deliberate: registry keys are otherwise tenant names, which
// are DNS labels and cannot contain one, so this cannot collide with a tenant or
// be mistaken for one by the per-tenant delete path.
const kernelMailRegistryKey = "_kernel"

// ensureKernelMail gives the cluster admin a working mailbox.
//
// Everything else in this file is driven by a Tenant, so on a cluster with no
// tenants none of it runs: the kernel domain was never a virtual mailbox domain,
// the kernel realm never appeared in Dovecot's auth config, and the introspection
// client was only ever created in tenant realms. The cluster admin lives in the
// kernel realm with an address at the kernel domain, so all three are needed
// before that mailbox works — and none of them depend on a tenant existing.
func (r *TenantReconciler) ensureKernelMail(ctx context.Context) error {
	if r.KernelDomain == "" {
		return fmt.Errorf("KERNEL_DOMAIN is empty; cannot provision kernel-realm mail")
	}

	// 1. Accept mail for the kernel domain.
	if err := r.ensureRegistryDomain(ctx, kernelMailRegistryKey, r.KernelDomain); err != nil {
		return fmt.Errorf("register kernel mail domain: %w", err)
	}

	// 2-3. Dovecot, only where Dovecot exists — as in ensureMail.
	//
	// Step 1 above stays unconditional: accepting mail for the kernel domain is
	// Postfix's registry, and Postfix runs in both modes.
	if !r.dovecotDeployed(ctx) {
		return nil
	}

	// 2. The introspection client in the kernel realm. Returns false while its Job
	//    is still running; the next operator start or tenant event picks it up.
	if ready, err := r.ensureKernelDovecotOIDCClientJob(ctx); err != nil {
		return fmt.Errorf("ensure kernel Dovecot OIDC client: %w", err)
	} else if !ready {
		return nil
	}

	// 3. Kernel realm in Dovecot's XOAUTH2 configuration, then reload if changed.
	if err := r.ensureDovecotRealmAuth(ctx, r.KernelRealm); err != nil {
		return fmt.Errorf("configure kernel realm auth: %w", err)
	}
	return r.ensureDovecotAuthReload(ctx)
}

// ensureRegistryDomain upserts one key in the shared tenant domain registry.
func (r *TenantReconciler) ensureRegistryDomain(ctx context.Context, key, domain string) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: mailPostfixVirtualDomainsConfigMap, Namespace: kernelNamespace}, cm)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mailPostfixVirtualDomainsConfigMap,
				Namespace: kernelNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: map[string]string{key: domain},
		})
	}
	if err != nil {
		return err
	}
	if cm.Data[key] == domain {
		return nil
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[key] = domain
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

// dovecotRealmAuthFiles renders the two Dovecot files that let one Keycloak realm
// authenticate IMAP.
//
// Dovecot's oauth2 passdb takes a single introspection URL, so multi-realm support
// is multiple passdb blocks rather than one parameterised block. Each realm gets a
// `<realm>.conf` holding the passdb and a `<realm>.oauth2.ext` holding its
// settings; dovecot.conf includes only *.conf, so the .ext files are read via the
// passdb args and never included as configuration themselves.
//
// result_failure = continue is what makes several realms work at all. Dovecot
// stops at the first passdb that answers definitively, so without it a token from
// the second realm would be rejected by the first realm's introspection and never
// reach its own.
func dovecotRealmAuthFiles(realmName, keycloakURL, clientSecret string) (passdbConf string, ext string) {
	passdbConf = fmt.Sprintf(`# Realm %[1]s — generated by the gentian-os mail reconciler. Do not edit.
passdb {
  driver = oauth2
  mechanisms = xoauth2
  args = %[2]s/%[1]s.oauth2.ext
  result_failure = continue
  result_internalfail = continue
}
`, realmName, dovecotRealmAuthMountPath)

	ext = fmt.Sprintf(`introspection_mode = post
introspection_url = %s/realms/%s/protocol/openid-connect/token/introspect
client_id = %s
client_secret = %s
username_attribute = email
active_attribute = active
active_value = true
`, strings.TrimSuffix(keycloakURL, "/"), realmName, dovecotOIDCClientID, clientSecret)
	return passdbConf, ext
}

// ensureDovecotRealmAuth adds one realm's XOAUTH2 configuration to the shared
// Dovecot auth Secret, keyed by realm.
//
// A Secret, not a ConfigMap, because the files carry the client secret Dovecot
// authenticates to Keycloak with.
//
// Keyed by realm, because the object this replaced held a single "values.yaml" and
// rewrote it on every tenant reconcile — so with two tenants the last one to
// reconcile owned the only introspection target and the other tenant's users could
// not log in. Its shape assumed one tenant.
func (r *TenantReconciler) ensureDovecotRealmAuth(ctx context.Context, realmName string) error {
	if realmName == "" {
		return fmt.Errorf("realm name is empty")
	}

	keycloakURL, err := r.secretValue(ctx, keycloakAdminSecret, kernelNamespace, "url")
	if err != nil {
		return fmt.Errorf("read Keycloak URL: %w", err)
	}
	clientSecret, err := r.secretValue(ctx, dovecotAdminSecretName, kernelNamespace, "oidc_client_secret")
	if err != nil {
		return fmt.Errorf("read Dovecot OIDC client secret: %w", err)
	}

	passdbConf, ext := dovecotRealmAuthFiles(realmName, keycloakURL, clientSecret)
	confKey := realmName + ".conf"
	extKey := realmName + ".oauth2.ext"

	secret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: dovecotRealmAuthSecret, Namespace: servicesNamespace}, secret)
	if errors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dovecotRealmAuthSecret,
				Namespace: servicesNamespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			StringData: map[string]string{confKey: passdbConf, extKey: ext},
		})
	}
	if err != nil {
		return err
	}
	if string(secret.Data[confKey]) == passdbConf && string(secret.Data[extKey]) == ext {
		return nil
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[confKey] = []byte(passdbConf)
	secret.Data[extKey] = []byte(ext)
	return r.Update(ctx, secret)
}

// ensureDovecotAuthReload makes Dovecot pick up a realm that was just added.
//
// Dovecot reads its passdb blocks once, at startup, so a new realm file appearing
// in the mount changes nothing until the process restarts — unlike the Postfix
// domain maps, which are texthash: files re-read per lookup and therefore
// restart-free. Stamping a hash of the realm set on the Pod template is what turns
// "the Secret changed" into "the Pod restarts", and it is a no-op once the hash
// matches, so this does not restart Dovecot on every reconcile.
//
// The Deployment is Argo CD-managed, so the 09-infra-helm ApplicationSet excludes
// this annotation from its diff; without that exclusion Argo would revert the
// stamp and the two would fight indefinitely.
func (r *TenantReconciler) ensureDovecotAuthReload(ctx context.Context) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: dovecotRealmAuthSecret, Namespace: servicesNamespace}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write(secret.Data[k])
	}
	want := hex.EncodeToString(h.Sum(nil))[:16]

	stage := envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
	deploy := &appsv1.Deployment{}
	name := fmt.Sprintf("dovecot-%s", stage)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: servicesNamespace}, deploy); err != nil {
		// Not an error: on a cluster where kernel mail is not deployed there is no
		// Dovecot to reload, and tenant reconcile must not block on its absence.
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if deploy.Spec.Template.Annotations[dovecotRealmAuthHashAnnotation] == want {
		return nil
	}

	// Patched, not updated. Two reasons, and either alone is sufficient.
	//
	// Argo CD owns this Deployment, so writing the whole object back from a read
	// that is already stale is how a concurrent sync gets clobbered. A merge
	// patch carries the one annotation this reconciler owns and nothing else.
	//
	// The operator is granted get/list/watch/patch on deployments and not
	// update, so the full write was refused outright:
	//
	//   reload Dovecot auth config: deployments.apps "dovecot-prod" is
	//   forbidden: User "system:serviceaccount:gentian-system:gentian-os"
	//   cannot update resource "deployments"
	//
	// which left MailReady=False on every tenant. Widening the grant would have
	// worked; not needing it is better.
	patch := client.MergeFrom(deploy.DeepCopy())
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations[dovecotRealmAuthHashAnnotation] = want
	return r.Patch(ctx, deploy, patch)
}

// secretValue reads one key from a Secret, treating an empty value as an error:
// an empty introspection URL or client secret produces a Dovecot config that
// parses and then fails every login.
func (r *TenantReconciler) secretValue(ctx context.Context, name, namespace, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", fmt.Errorf("get Secret %s/%s: %w", namespace, name, err)
	}
	v := strings.TrimSpace(string(secret.Data[key]))
	if v == "" {
		return "", fmt.Errorf("secret %s/%s has no %s", namespace, name, key)
	}
	return v, nil
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
		// The LMTP map is derived from that registry, so it has to follow the
		// removal — otherwise Postfix keeps delivering to a domain it no longer
		// accepts mail for.
		if err := r.syncPostfixVirtualMailboxMaps(ctx); err != nil {
			return fmt.Errorf("sync Postfix virtual mailbox maps for tenant %s: %w", tenant.Name, err)
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
	pub, _, err := r.ensureDKIMKeyPair(ctx, dkimSecretName(tenant.Name), map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	return pub, err
}

// ensureDKIMKeyPair creates or reads a DKIM key Secret by name, returning the
// base64 PKIX public key for the DNS record and the PEM private key for Postfix.
//
// Named rather than tenant-scoped because the kernel domain needs a key on the
// same terms as a tenant: generated once, never rotated, and published from the
// same value that signs. Leaving the kernel domain's key to the image instead
// meant the image generated a new one whenever it restarted, so the record
// published for the old key stayed in DNS and every kernel-domain signature
// failed verification — a state strictly worse than not signing at all.
func (r *TenantReconciler) ensureDKIMKeyPair(ctx context.Context, secretName string, labels map[string]string) (string, []byte, error) {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: kernelNamespace}, existing)
	if err == nil {
		// Secret already exists — derive the public key from the stored private key.
		privPEM, ok := existing.Data["tls.key"]
		if !ok {
			return "", nil, fmt.Errorf("DKIM secret %s/%s is missing key tls.key", kernelNamespace, secretName)
		}
		block, _ := pem.Decode(privPEM)
		if block == nil {
			return "", nil, fmt.Errorf("DKIM secret %s/%s: tls.key is not valid PEM", kernelNamespace, secretName)
		}
		priv, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse DKIM private key in %s/%s: %w", kernelNamespace, secretName, parseErr)
		}
		pubDER, marshalErr := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if marshalErr != nil {
			return "", nil, fmt.Errorf("marshal DKIM public key for %s/%s: %w", kernelNamespace, secretName, marshalErr)
		}
		return base64.StdEncoding.EncodeToString(pubDER), privPEM, nil
	}
	if !errors.IsNotFound(err) {
		return "", nil, err
	}

	// Generate a fresh RSA-2048 key pair.
	priv, err := rsa.GenerateKey(rand.Reader, dkimKeySize)
	if err != nil {
		return "", nil, fmt.Errorf("generate DKIM RSA key for %s: %w", secretName, err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", nil, fmt.Errorf("marshal DKIM public key for %s: %w", secretName, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: kernelNamespace,
			Labels:    labels,
		},
		Data: map[string][]byte{
			"tls.key": privPEM,
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", nil, fmt.Errorf("create DKIM secret %s/%s: %w", kernelNamespace, secretName, err)
	}
	return base64.StdEncoding.EncodeToString(pubDER), privPEM, nil
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
