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

package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TestMail_Disabled verifies that a Tenant with mail.mode=disabled immediately
// sets MailReady=True with reason MailDisabled and requires no Application CRs.
func TestMail_Disabled(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "maildisabled"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Disabled Co",
			Domain:      "maildisabled.example.com",
			AdminEmail:  "admin@maildisabled.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeDisabled},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
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

	// No Postfix or Dovecot Application CRs should have been created.
	postfixApp := &unstructured.Unstructured{}
	postfixApp.SetGroupVersionKind(argocdAppGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "postfix-maildisabled", Namespace: "argocd"}, postfixApp); err == nil {
		t.Error("unexpected Postfix Application CR found for disabled mail mode")
	}
}

// TestMail_Selfhosted_CreatesApplicationsAndDKIM verifies that mail.mode=selfhosted
// creates Postfix and Dovecot ArgoCD Application CRs and a DKIM key Secret in the
// tenant namespace.
func TestMail_Selfhosted_CreatesApplicationsAndDKIM(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailself"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Selfhosted Co",
			Domain:      "mailself.example.com",
			AdminEmail:  "admin@mailself.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for Postfix Application CR.
	postfixApp := &unstructured.Unstructured{}
	postfixApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "postfix-mailself", Namespace: "argocd"}, postfixApp) == nil
	})

	destNS, _, _ := unstructured.NestedString(postfixApp.Object, "spec", "destination", "namespace")
	if destNS != "tenant-mailself" {
		t.Errorf("expected Postfix destination namespace tenant-mailself, got %q", destNS)
	}
	if postfixApp.GetLabels()["gentianos.io/tenant"] != "mailself" {
		t.Errorf("expected tenant label 'mailself' on Postfix app, got %q",
			postfixApp.GetLabels()["gentianos.io/tenant"])
	}

	// Wait for Dovecot Application CR.
	dovecotApp := &unstructured.Unstructured{}
	dovecotApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "dovecot-mailself", Namespace: "argocd"}, dovecotApp) == nil
	})

	destNS2, _, _ := unstructured.NestedString(dovecotApp.Object, "spec", "destination", "namespace")
	if destNS2 != "tenant-mailself" {
		t.Errorf("expected Dovecot destination namespace tenant-mailself, got %q", destNS2)
	}

	// Wait for DKIM Secret in the tenant namespace.
	dkimSecret := &corev1.Secret{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "dkim-mailself", Namespace: "tenant-mailself"}, dkimSecret) == nil
	})

	privPEM, ok := dkimSecret.Data["tls.key"]
	if !ok || len(privPEM) == 0 {
		t.Error("expected non-empty tls.key in DKIM secret")
	}

	// TenantStatus.Mail must carry the DKIM public key and DNS record suggestions.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailself"}, updated)
		return updated.Status.Mail != nil && updated.Status.Mail.DKIMPublicKey != ""
	})

	if updated.Status.Mail.SPFRecord == "" {
		t.Error("expected non-empty SPFRecord in tenant status")
	}
	if updated.Status.Mail.DMARCRecord == "" {
		t.Error("expected non-empty DMARCRecord in tenant status")
	}
}

// TestMail_DovecotHasLDAPConfig verifies that the Dovecot ArgoCD Application CR carries
// the LDAP account provisioner helm values read from the udm-admin Secret.
func TestMail_DovecotHasLDAPConfig(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "maildoveldap"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Dovecot LDAP Co",
			Domain:      "maildoveldap.example.com",
			AdminEmail:  "admin@maildoveldap.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	dovecotApp := &unstructured.Unstructured{}
	dovecotApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "dovecot-maildoveldap", Namespace: "argocd"}, dovecotApp) == nil
	})

	helmValues, _, _ := unstructured.NestedString(dovecotApp.Object, "spec", "source", "helm", "values")
	if helmValues == "" {
		t.Fatal("expected non-empty helm values on Dovecot Application CR")
	}
	for _, want := range []string{
		"ACCOUNT_PROVISIONER: LDAP",
		"nubus-dev-ldap-server.gentian-dev.svc.cluster.local",
		"dc=swp-ldap,dc=internal",
		"ldapsearch_dovecot",
		"mail.maildoveldap.example.com",
	} {
		if !strings.Contains(helmValues, want) {
			t.Errorf("expected helm values to contain %q, got:\n%s", want, helmValues)
		}
	}
}

// TestMail_DefaultMode_IsSelfhosted verifies that a Tenant with no mail spec defaults to
// selfhosted mode, creating Postfix and Dovecot Application CRs.
func TestMail_DefaultMode_IsSelfhosted(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "maildefault"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Default Co",
			Domain:      "maildefault.example.com",
			AdminEmail:  "admin@maildefault.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	postfixApp := &unstructured.Unstructured{}
	postfixApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "postfix-maildefault", Namespace: "argocd"}, postfixApp) == nil
	})

	dovecotApp := &unstructured.Unstructured{}
	dovecotApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "dovecot-maildefault", Namespace: "argocd"}, dovecotApp) == nil
	})
}

// TestMail_TransportOnly_CreatesOnlyPostfix verifies that mail.mode=transport-only
// creates a Postfix Application CR but not a Dovecot Application CR.
func TestMail_TransportOnly_CreatesOnlyPostfix(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailrelay"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail Relay Co",
			Domain:      "mailrelay.example.com",
			AdminEmail:  "admin@mailrelay.example.com",
			Mail:        &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeTransportOnly},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	postfixApp := &unstructured.Unstructured{}
	postfixApp.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "postfix-mailrelay", Namespace: "argocd"}, postfixApp) == nil
	})

	// Give the reconciler time to settle, then assert Dovecot is absent.
	time.Sleep(500 * time.Millisecond)
	dovecotApp := &unstructured.Unstructured{}
	dovecotApp.SetGroupVersionKind(argocdAppGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "dovecot-mailrelay", Namespace: "argocd"}, dovecotApp); err == nil {
		t.Error("unexpected Dovecot Application CR found for transport-only mode")
	}
}

// TestMail_External_MissingConfig verifies that mail.mode=external with no
// smtpCredentialsSecret sets MailReady=False with reason MissingConfig.
func TestMail_External_MissingConfig(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mailextnotconf"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Mail External No Config",
			Domain:      "mailextnotconf.example.com",
			AdminEmail:  "admin@mailextnotconf.example.com",
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
	waitFor(t, 10*time.Second, func() bool {
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
			AdminEmail:  "admin@mailexternal.example.com",
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
	waitFor(t, 10*time.Second, func() bool {
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
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mailexternal"}, updated)
		cond := findCondition(updated, "MailReady")
		return cond != nil && cond.Status == metav1.ConditionTrue
	})
	if cond := findCondition(updated, "MailReady"); cond.Reason != "External" {
		t.Errorf("expected reason External, got %q", cond.Reason)
	}
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
