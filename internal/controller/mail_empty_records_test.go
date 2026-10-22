/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// mailingProfile is an app that sends mail and reads it.
func mailingProfile() *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "mailer"
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Mail: &gentianov1alpha1.MailRequirement{
			SMTP: &gentianov1alpha1.SMTPRequirement{},
			IMAP: &gentianov1alpha1.IMAPRequirement{},
		},
	}}
	return p
}

func mailingTenant() *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "quiet"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Quiet Co",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "mailer"}},
		},
	}
}

// An app that declares mail installs on a cluster without mail: its records
// are there, every value empty, so the ExternalSecret that reads them syncs.
func TestMail_NoMailFunctionStillWritesEmptyAppRecords(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).
		WithObjects(mailingProfile(), tenant).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Seeder: secrets.NewSeeder(vault, nil)}

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	smtp, ok := vault[secrets.CategoryPath("quiet", "mailer", "smtp")]
	if !ok {
		t.Fatal("no SMTP record: the app's ExternalSecret names a path nothing wrote")
	}
	for _, k := range []string{"host", "port", "user", "password"} {
		if v, ok := smtp[k]; !ok || v != "" {
			t.Errorf("smtp %s = %q (present %v), want present and empty", k, v, ok)
		}
	}
	imap, ok := vault[secrets.CategoryPath("quiet", "mailer", "imap")]
	if !ok {
		t.Fatal("no IMAP record")
	}
	for _, k := range []string{"host", "port"} {
		if v, ok := imap[k]; !ok || v != "" {
			t.Errorf("imap %s = %q (present %v), want present and empty", k, v, ok)
		}
	}
}

// Mail that arrives later replaces the empty record, and a pass without mail
// afterwards leaves the real one alone.
func TestMail_RealRecordReplacesEmptyAndIsNotEmptiedAgain(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).
		WithObjects(mailingProfile(), tenant).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Seeder: secrets.NewSeeder(vault, nil), KernelDomain: "example.org"}

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	// The cluster gains its mail function, the tenant a relay.
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mailNamespace}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "relay", Namespace: mailNamespace},
		Data: map[string][]byte{
			"host": []byte("smtp.example.com"), "port": []byte("587"),
			"username": []byte("noreply@example.com"), "password": []byte("s3cret"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	tenant.Spec.Mail = &gentianov1alpha1.TenantMail{
		Mode: gentianov1alpha1.MailModeExternal, SmtpCredentialsSecret: "relay",
	}
	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	smtpPath := secrets.CategoryPath("quiet", "mailer", "smtp")
	want := map[string]string{
		"host": "smtp.example.com", "port": "587",
		"user": "noreply@example.com", "password": "s3cret",
	}
	for k, v := range want {
		if got := vault[smtpPath][k]; got != v {
			t.Errorf("smtp %s = %q after mail arrived, want %q", k, got, v)
		}
	}
	if got := vault[secrets.CategoryPath("quiet", "mailer", "imap")]["host"]; got != "imap.example.org" {
		t.Errorf("imap host = %q after mail arrived, want imap.example.org", got)
	}

	// Mail goes away again: the empty record is not written over the real one.
	if err := c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mailNamespace}}); err != nil {
		t.Fatal(err)
	}
	tenant.Spec.Mail = nil
	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if got := vault[smtpPath][k]; got != v {
			t.Errorf("smtp %s = %q after a pass without mail, want %q kept", k, got, v)
		}
	}
}
