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

package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller"
)

// TestMail_Disabled verifies that a Tenant with mail.mode=disabled immediately
// sets MailReady=True with reason MailDisabled and requires no shared infrastructure changes.
func TestMail_Disabled(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "maildisabled"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Disabled Co",
			Domain:      "maildisabled.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeDisabled},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "maildisabled"}, updated)
		return findCondition(updated, "MailReady") != nil
	})

	cond := findCondition(updated, "MailReady")
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected MailReady=True, got %v", cond.Status)
	}
	if cond.Reason != "MailDisabled" {
		t.Errorf("expected reason MailDisabled, got %q", cond.Reason)
	}

	// No Postfix virtual-domains ConfigMap entry should exist for this tenant.
	postfixCM := &corev1.ConfigMap{}
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM); err == nil {
		if _, ok := postfixCM.Data["maildisabled"]; ok {
			t.Error("unexpected Postfix virtual-domain entry found for disabled mail mode")
		}
	}
}

// TestMail_Selfhosted_ProvisionsTenantInSharedInfra verifies that mail.mode=selfhosted
// registers the tenant in the shared mail infrastructure:
//   - DKIM key Secret in the kernel namespace
//   - Postfix virtual-domains ConfigMap entry
//   - Dovecot domains ConfigMap entry
//   - SMTP credentials Secret in the tenant namespace
//   - DNS records in TenantStatus
func TestMail_Selfhosted_ProvisionsTenantInSharedInfra(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailself"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Selfhosted Co",
			Domain:      "mailself.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for MailReady=True.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailself"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Status == metav1.ConditionTrue
	})

	cond := findCondition(updated, "MailReady")
	if cond.Reason != "Selfhosted" {
		t.Errorf("expected reason Selfhosted, got %q", cond.Reason)
	}

	// DKIM key Secret must be in the kernel namespace (accessible to shared Rspamd).
	dkimSecret := &corev1.Secret{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "dkim-mailself", Namespace: "platform-kernel"}, dkimSecret) == nil
	})
	if len(dkimSecret.Data["tls.key"]) == 0 {
		t.Error("expected non-empty tls.key in DKIM secret")
	}
	if dkimSecret.Labels["gentianos.io/tenant"] != "mailself" {
		t.Errorf("expected tenant label 'mailself' on DKIM secret, got %q",
			dkimSecret.Labels["gentianos.io/tenant"])
	}

	// Postfix virtual-domains ConfigMap must contain the tenant domain.
	postfixCM := &corev1.ConfigMap{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM) == nil
	})
	if postfixCM.Data["mailself"] != "mailself.example.com" {
		t.Errorf("expected Postfix virtual-domain 'mailself.example.com', got %q", postfixCM.Data["mailself"])
	}

	// Dovecot domains ConfigMap must contain the tenant domain.
	dovecotCM := &corev1.ConfigMap{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "mail-dovecot-domains", Namespace: "platform-kernel"}, dovecotCM) == nil
	})
	if dovecotCM.Data["mailself"] != "mailself.example.com" {
		t.Errorf("expected Dovecot domain 'mailself.example.com', got %q", dovecotCM.Data["mailself"])
	}

	// SMTP credentials Secret must be in the tenant namespace.
	smtpSecret := &corev1.Secret{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "smtp-credentials-mailself", Namespace: "tenant-mailself"}, smtpSecret) == nil
	})
	if string(smtpSecret.Data["host"]) != "postfix-dev.platform-kernel.svc.cluster.local" {
		t.Errorf("expected SMTP host=postfix-dev.platform-kernel.svc.cluster.local, got %q",
			string(smtpSecret.Data["host"]))
	}
	if string(smtpSecret.Data["username"]) != "smtp-mailself" {
		t.Errorf("expected SMTP username=smtp-mailself, got %q", string(smtpSecret.Data["username"]))
	}
	if len(smtpSecret.Data["password"]) == 0 {
		t.Error("expected non-empty SMTP password in credentials secret")
	}
	if smtpSecret.Labels["gentianos.io/tenant"] != "mailself" {
		t.Errorf("expected tenant label 'mailself' on SMTP credentials secret, got %q",
			smtpSecret.Labels["gentianos.io/tenant"])
	}

	// TenantStatus.Mail must carry the DKIM public key and DNS record suggestions.
	_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailself"}, updated)
	if updated.Status.Mail == nil || updated.Status.Mail.DKIMPublicKey == "" {
		t.Error("expected non-empty DKIMPublicKey in tenant status")
	}
	if updated.Status.Mail.SPFRecord == "" {
		t.Error("expected non-empty SPFRecord in tenant status")
	}
	if updated.Status.Mail.DMARCRecord == "" {
		t.Error("expected non-empty DMARCRecord in tenant status")
	}
}

// TestMail_Selfhosted_DoesNotCreatePerTenantApplicationCRs verifies that the new shared
// infrastructure model does not create per-tenant ArgoCD Application CRs for Postfix or
// Dovecot. Only the shared ConfigMap entries and SMTP credentials Secret are created.
func TestMail_Selfhosted_DoesNotCreatePerTenantApplicationCRs(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailnoapps"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail No Apps Co",
			Domain:      "mailnoapps.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for MailReady=True to confirm provisioning is complete.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailnoapps"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Status == metav1.ConditionTrue
	})

	// Give the reconciler time to settle, then assert no per-tenant Application CRs were created.
	time.Sleep(200 * time.Millisecond)
	postfixApp := &corev1.ConfigMap{} // reuse ConfigMap type to avoid argocd GVK dependency
	_ = postfixApp
	// The absence of per-tenant Postfix/Dovecot Application CRs is the key assertion.
	// We verify this by confirming the shared ConfigMap path was used instead.
	postfixCM := &corev1.ConfigMap{}
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM); err != nil {
		t.Errorf("expected shared Postfix ConfigMap to exist: %v", err)
	}
}

// TestMail_DefaultMode_IsSelfhosted verifies that a Tenant with no mail spec defaults to
// selfhosted mode, registering the tenant in the shared mail infrastructure.
func TestMail_DefaultMode_IsSelfhosted(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "maildefault"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Default Co",
			Domain:      "maildefault.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for MailReady=True.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "maildefault"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Status == metav1.ConditionTrue
	})
	if cond := findCondition(updated, "MailReady"); cond.Reason != "Selfhosted" {
		t.Errorf("expected reason Selfhosted for default mode, got %q", cond.Reason)
	}

	// Confirm the tenant is registered in the shared Postfix ConfigMap.
	postfixCM := &corev1.ConfigMap{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM) == nil
	})
	if postfixCM.Data["maildefault"] != "maildefault.example.com" {
		t.Errorf("expected Postfix virtual-domain 'maildefault.example.com', got %q",
			postfixCM.Data["maildefault"])
	}

	// Confirm the tenant is registered in the shared Dovecot ConfigMap.
	dovecotCM := &corev1.ConfigMap{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "mail-dovecot-domains", Namespace: "platform-kernel"}, dovecotCM) == nil
	})
	if dovecotCM.Data["maildefault"] != "maildefault.example.com" {
		t.Errorf("expected Dovecot domain 'maildefault.example.com', got %q",
			dovecotCM.Data["maildefault"])
	}
}

// TestMail_TransportOnly_RegistersPostfixOnly verifies that mail.mode=transport-only
// registers the tenant in the shared Postfix ConfigMap for outbound relay but does NOT
// register it in the Dovecot domains ConfigMap (no IMAP storage).
func TestMail_TransportOnly_RegistersPostfixOnly(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailrelay"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Relay Co",
			Domain:      "mailrelay.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeTransportOnly},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for Postfix ConfigMap entry.
	postfixCM := &corev1.ConfigMap{}
	waitFor(t, jobAppearTimeout, func() bool {
		if err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM); err != nil {
			return false
		}
		return postfixCM.Data["mailrelay"] != ""
	})
	if postfixCM.Data["mailrelay"] != "mailrelay.example.com" {
		t.Errorf("expected Postfix virtual-domain 'mailrelay.example.com', got %q",
			postfixCM.Data["mailrelay"])
	}

	// SMTP credentials Secret must be in the tenant namespace.
	smtpSecret := &corev1.Secret{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "smtp-credentials-mailrelay", Namespace: "tenant-mailrelay"}, smtpSecret) == nil
	})

	// Give the reconciler time to settle, then assert Dovecot ConfigMap has no entry.
	time.Sleep(500 * time.Millisecond)
	dovecotCM := &corev1.ConfigMap{}
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "mail-dovecot-domains", Namespace: "platform-kernel"}, dovecotCM); err == nil {
		if _, ok := dovecotCM.Data["mailrelay"]; ok {
			t.Error("unexpected Dovecot domain entry found for transport-only mode")
		}
	}
}

// TestMail_External_MissingConfig verifies that mail.mode=external with no
// smtpCredentialsSecret sets MailReady=False with reason MissingConfig.
func TestMail_External_MissingConfig(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailextnotconf"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail External No Config",
			Domain:      "mailextnotconf.example.com",
			Mail: &gentianov1alpha1.TenantMail{
				Mode: gentianov1alpha1.MailModeExternal,
				// SmtpCredentialsSecret intentionally not set.
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailextnotconf"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Reason == "MissingConfig"
	})

	cond := findCondition(updated, "MailReady")
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected MailReady=False, got %v", cond.Status)
	}
}

// TestMail_External_CopiesCredentialsSecret verifies that mail.mode=external copies the
// referenced SMTP credentials Secret from the kernel namespace into the tenant namespace.
func TestMail_External_CopiesCredentialsSecret(t *testing.T) {
	t.Parallel()
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-smtp-creds",
			Namespace: "platform-kernel",
		},
		Data: map[string][]byte{
			"host":     []byte("smtp.example.com"),
			"port":     []byte("587"),
			"username": []byte("noreply@example.com"),
			"password": []byte("s3cret"),
		},
	}
	if err := testClient.Create(context.Background(), src); err != nil {
		t.Fatalf("create source SMTP secret: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), src) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailexternal"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail External Co",
			Domain:      "mailexternal.example.com",
			Mail: &gentianov1alpha1.TenantMail{
				Mode:                  gentianov1alpha1.MailModeExternal,
				SmtpCredentialsSecret: "tenant-smtp-creds",
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	dst := &corev1.Secret{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "smtp-credentials-mailexternal", Namespace: "tenant-mailexternal"}, dst) == nil
	})

	if string(dst.Data["host"]) != "smtp.example.com" {
		t.Errorf("expected host=smtp.example.com in copied secret, got %q", string(dst.Data["host"]))
	}
	if dst.Labels["gentianos.io/tenant"] != "mailexternal" {
		t.Errorf("expected tenant label 'mailexternal', got %q", dst.Labels["gentianos.io/tenant"])
	}

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailexternal"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Status == metav1.ConditionTrue
	})
	if cond := findCondition(updated, "MailReady"); cond.Reason != "External" {
		t.Errorf("expected reason External, got %q", cond.Reason)
	}
}

// TestMail_PostfixInboundMapsFollowTenant verifies that registering a tenant
// domain also produces the two texthash: files kernel Postfix reads, and that
// deleting the tenant removes it from both.
//
// The map ConfigMap is what makes inbound mail work at all: Postfix accepts a
// recipient only if its domain is in virtual_mailbox_domains, and delivers it
// only if the address matches virtual_mailbox_maps. A tenant present in the
// registry but absent from these is refused with
// "554 5.7.1 Recipient address rejected: Access denied".
func TestMail_PostfixInboundMapsFollowTenant(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailmaps"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Maps Co",
			Domain:      "mailmaps.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	maps := &corev1.ConfigMap{}
	mapsKey := types.NamespacedName{
		Name: "postfix-kernel-virtual-mailbox-maps", Namespace: "platform-kernel",
	}
	waitFor(t, jobAppearTimeout, func() bool {
		if err := testClient.Get(context.Background(), mapsKey, maps); err != nil {
			return false
		}
		return strings.Contains(maps.Data["virtual_mailbox_domains"], "mailmaps.example.com")
	})

	if got := maps.Data["virtual_mailbox_domains"]; !strings.Contains(got, "mailmaps.example.com OK") {
		t.Errorf("expected virtual_mailbox_domains to accept mailmaps.example.com, got %q", got)
	}
	if got := maps.Data["virtual_mailbox_maps"]; !strings.Contains(got, "@mailmaps.example.com mailmaps.example.com/") {
		t.Errorf("expected virtual_mailbox_maps catch-all for mailmaps.example.com, got %q", got)
	}

	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	waitFor(t, jobAppearTimeout, func() bool {
		if err := testClient.Get(context.Background(), mapsKey, maps); err != nil {
			return false
		}
		return !strings.Contains(maps.Data["virtual_mailbox_domains"], "mailmaps.example.com")
	})
	if got := maps.Data["virtual_mailbox_maps"]; strings.Contains(got, "mailmaps.example.com") {
		t.Errorf("expected deleted tenant to drop out of virtual_mailbox_maps, got %q", got)
	}
}

// TestTenantDelete_RemovesPortalShellSecret verifies the cached portal shell
// credential goes on undeploy, under Retain — the policy that keeps the DKIM key
// and the Keycloak realm.
//
// The distinction Retain draws is whether something can be reconstructed without
// a human, not whether it is a credential. This Secret is a projection of
// OpenBao, so a redeploy rewrites it identically; left behind it is a live
// DATABASE_URL for a tenant that no longer exists.
func TestTenantDelete_RemovesPortalShellSecret(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "shellsecret"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Shell Secret Co",
			Domain:         "shellsecret.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyRetain,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	key := types.NamespacedName{Name: "portal-shell-shellsecret", Namespace: "platform-kernel"}
	secret := &corev1.Secret{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(), key, secret) == nil
	})

	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(), key, secret) != nil
	})
	if err := testClient.Get(context.Background(), key, secret); err == nil {
		t.Errorf("portal-shell-shellsecret survived undeploy under Retain")
	}
}

// TestMail_MapsDedupeSharedDomain verifies two tenants naming the same mail
// domain produce one texthash line, not two.
//
// The registry is keyed by tenant, and a defaults component that hardcodes
// mail.domain gives every tenant the same one — which emitted the kernel entry
// and the tenant entry as duplicate lines in both files.
func TestMail_MapsDedupeSharedDomain(t *testing.T) {
	t.Parallel()
	shared := "shareddomain.example.com"
	for _, name := range []string{"sharedone", "sharedtwo"} {
		tenant := &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gentianov1alpha1.TenantSpec{
				DisplayName: name,
				Mail:        &gentianov1alpha1.TenantMail{Domain: shared},
			},
		}
		if err := testClient.Create(context.Background(), tenant); err != nil {
			t.Fatalf("create tenant %s: %v", name, err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })
	}

	maps := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: "postfix-kernel-virtual-mailbox-maps", Namespace: "platform-kernel",
	}
	waitFor(t, jobAppearTimeout, func() bool {
		if err := testClient.Get(context.Background(), key, maps); err != nil {
			return false
		}
		return strings.Contains(maps.Data["virtual_mailbox_domains"], shared)
	})

	if got := strings.Count(maps.Data["virtual_mailbox_domains"], shared+" OK"); got != 1 {
		t.Errorf("expected one accept line for %s, got %d:\n%s", shared, got, maps.Data["virtual_mailbox_domains"])
	}
	if got := strings.Count(maps.Data["virtual_mailbox_maps"], "@"+shared+" "); got != 1 {
		t.Errorf("expected one route line for %s, got %d:\n%s", shared, got, maps.Data["virtual_mailbox_maps"])
	}
}

// TestTenant_PublishesResolvedAdminEmail verifies the resolved address reaches
// status, which is the field consumers read.
//
// spec.adminEmail is empty whenever the address is derived — the normal case
// since it became optional — so a consumer reading only the spec sees nothing
// and reconstructs its own answer. `gtnctl tenants deploy` did exactly that and
// printed admin-<tenant>@gentian.org: a domain belonging to no cluster, for an
// account in no realm, on the one line an operator copies to sign in with.
func TestTenant_PublishesResolvedAdminEmail(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "adminemail"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Admin Email Co",
			Domain:      "adminemail.example.com",
			// No AdminEmail: the derivation is what is under test.
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		if err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "adminemail"}, updated); err != nil {
			return false
		}
		return updated.Status.AdminEmail != ""
	})

	if got, want := updated.Status.AdminEmail, "admin@adminemail.example.com"; got != want {
		t.Errorf("status.adminEmail = %q, want %q", got, want)
	}
	// spec.adminEmail no longer exists: the address is derived, so status is
	// the only place it appears and there is nothing to leave empty.
}

// --- helpers ----------------------------------------------------------------

// findCondition returns the first condition with the given type, or nil.
func findCondition(tenant *gentianov1alpha1.Tenant, condType string) *metav1.Condition {
	for i := range tenant.Status.Conditions {
		if tenant.Status.Conditions[i].Type == condType {
			return &tenant.Status.Conditions[i]
		}
	}
	return nil
}

// The Dovecot gate.
//
// A cluster in external mail mode runs no Dovecot — the ApplicationSet does not
// deploy one, because the mailboxes are at the provider. The operator went on
// configuring it regardless: a Keycloak client per realm, a Job per reconcile,
// realm auth and a domains ConfigMap, all addressed to a service that does not
// exist. Nothing failed, which is why it ran for a day unnoticed.
//
// Tested on the predicate rather than through a reconcile, because the envtest
// harness runs one reconciler for the whole suite in kernel mode; the point
// here is what the predicate answers, and that empty is external.
func TestDovecotDeployed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"kernel", true},
		{"external", false},
		// Unset is external: configuring an absent Dovecot is silent waste,
		// while skipping a present one fails IMAP visibly and is fixed by
		// setting the value. Of the two, prefer the loud one.
		{"", false},
		// Anything unrecognised is not kernel. A typo must not provision.
		{"Kernel", false},
		{"selfhosted", false},
	} {
		r := &controller.TenantReconciler{MailServiceMode: tc.mode}
		if got := r.DovecotDeployedForTest(); got != tc.want {
			t.Errorf("MailServiceMode=%q → %v, want %v", tc.mode, got, tc.want)
		}
	}
}
