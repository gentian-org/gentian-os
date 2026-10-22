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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// relaySecret is keycloak-smtp-credentials as its ExternalSecret renders it,
// with the relay credential supplied when user and password are not empty.
func relaySecret(user, password string) *corev1.Secret {
	configure := "false"
	if user != "" && password != "" {
		configure = "true"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keycloakSMTPCredentialsSecret, Namespace: identityNamespace},
		Data: map[string][]byte{
			"mail_service_mode": []byte("external"),
			"smtp_host":         []byte("smtp.gmail.com"),
			"smtp_port":         []byte("587"),
			"smtp_starttls":     []byte("true"),
			"smtp_ssl":          []byte("false"),
			"smtp_user":         []byte(user),
			"smtp_password":     []byte(password),
			"smtp_configure":    []byte(configure),
		},
	}
}

func relayReconciler(t *testing.T, mode string, objs ...client.Object) (*TenantReconciler, client.Client, memVault) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).WithObjects(objs...).Build()
	vault := memVault{}
	return &TenantReconciler{
		Client: c, Seeder: secrets.NewSeeder(vault, nil),
		KernelDomain: "example.org", MailServiceMode: mode,
	}, c, vault
}

func wantSMTP(t *testing.T, vault memVault, want map[string]string) {
	t.Helper()
	got := vault[secrets.CategoryPath("quiet", "mailer", "smtp")]
	for _, k := range []string{"host", "port", "user", "password"} {
		if got[k] != want[k] {
			t.Errorf("smtp %s = %q, want %q", k, got[k], want[k])
		}
	}
}

var emptySMTP = map[string]string{"host": "", "port": "", "user": "", "password": ""}

// A cluster that relays and composes no mail function gives each app the
// relay's address and login, and keeps the IMAP record empty.
func TestMail_ExternalRelayFillsAppSMTPRecord(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	r, _, vault := relayReconciler(t, "external", mailingProfile(), tenant, relaySecret("relay@example.com", "s3cret"))

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	wantSMTP(t, vault, map[string]string{
		"host": "smtp.gmail.com", "port": "587", "user": "relay@example.com", "password": "s3cret",
	})
	imap := vault[secrets.CategoryPath("quiet", "mailer", "imap")]
	if imap == nil || imap["host"] != "" || imap["port"] != "" {
		t.Errorf("imap record = %v, want present and empty", imap)
	}
}

// Until the relay credential is supplied the record stays empty, and once a
// record holds values an unsupplied credential does not empty it.
func TestMail_ExternalRelayWithoutCredentialKeepsRecord(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	r, c, vault := relayReconciler(t, "external", mailingProfile(), tenant, relaySecret("", ""))

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	wantSMTP(t, vault, emptySMTP)

	sec := relaySecret("relay@example.com", "s3cret")
	if err := c.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	filled := map[string]string{
		"host": "smtp.gmail.com", "port": "587", "user": "relay@example.com", "password": "s3cret",
	}
	wantSMTP(t, vault, filled)

	if err := c.Update(ctx, relaySecret("", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	wantSMTP(t, vault, filled)
}

// A changed relay login reaches the records already written.
func TestMail_ExternalRelayChangeUpdatesRecord(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	r, c, vault := relayReconciler(t, "external", mailingProfile(), tenant, relaySecret("old@example.com", "old"))

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, relaySecret("new@example.com", "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	wantSMTP(t, vault, map[string]string{
		"host": "smtp.gmail.com", "port": "587", "user": "new@example.com", "password": "new",
	})
}

// A cluster that runs its own mail does not hand out the relay's login: with
// no mail function composed yet, the record stays empty as before.
func TestMail_SystemModeDoesNotUseRelay(t *testing.T) {
	ctx := context.Background()
	tenant := mailingTenant()
	r, _, vault := relayReconciler(t, mailServiceModeSystem, mailingProfile(), tenant, relaySecret("relay@example.com", "s3cret"))

	if _, err := r.ensureMail(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	wantSMTP(t, vault, emptySMTP)
}
