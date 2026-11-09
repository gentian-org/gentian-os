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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The table Postfix reads to decide who may send as which address, as the
// operator writes it from the mail registry.
func TestSenderLoginMapFromTheRegistry(t *testing.T) {
	w := newMailboxWorld(t, mailRegistry(map[string]string{
		kernelMailRegistryKey: "k.example",
		"team":                "team.k.example",
		"acme":                "acme.com",
	}), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keycloakSMTPCredentialsSecret, Namespace: identityNamespace},
		Data:       map[string][]byte{"smtp_user": []byte("gentian-system@k.example")},
	})
	want := `/^noreply@k\.example$/ noreply@k.example,gentian-system@k.example,noreply@acme.com,noreply@team.k.example
/^([^@]+)@acme\.com$/ ${1}@acme.com,smtp-acme
/^([^@]+)@k\.example$/ ${1}@k.example
/^([^@]+)@team\.k\.example$/ ${1}@team.k.example,smtp-team
`
	if got := w.postfixMaps()[postfixSenderLoginKey]; got != want {
		t.Fatalf("sender login map:\n%s\nwant:\n%s", got, want)
	}
}

// A tenant on the kernel domain itself (single tenancy) shares the domain with
// the kernel: one line, both owners, and its realm's noreply is the kernel's.
func TestSenderLoginMapSharedKernelDomain(t *testing.T) {
	got := renderSenderLoginMap(senderOwners(map[string]string{
		kernelMailRegistryKey: "k.example",
		"user":                "K.example",
	}, "k.example", "", ""))
	want := `/^noreply@k\.example$/ noreply@k.example
/^([^@]+)@k\.example$/ ${1}@k.example,smtp-user
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// No registry yet: the kernel domain's own lines are still written, so the
// file exists and the kernel realm can send from the first start.
func TestSenderLoginMapEmptyRegistry(t *testing.T) {
	w := newMailboxWorld(t)
	want := `/^noreply@k\.example$/ noreply@k.example
/^([^@]+)@k\.example$/ ${1}@k.example
`
	if got := w.postfixMaps()[postfixSenderLoginKey]; got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// clusterConfig is the ConfigMap the Cluster composition writes, with the
// given keys.
func clusterConfig(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterConfigName, Namespace: clusterConfigNamespace},
		Data:       data,
	}
}

// The cluster's mail tenant: its people receive at the cluster's domain in
// the mailbox they already have, and send from it. The kernel realm's people
// keep their own mailboxes, and so do the role addresses.
func TestTheClustersDomainGoesToItsMailTenant(t *testing.T) {
	w := newMailboxWorld(t, mailRegistry(map[string]string{
		kernelMailRegistryKey: "k.example",
		"team":                "team.k.example",
		"acme":                "acme.com",
	}), clusterConfig(map[string]string{clusterConfigMailTenantKey: "team"}))
	w.tr.MailAdminContact = "ops@outside.example"
	w.people.set("kernel", person("admin@k.example", true), person("abuse@k.example", true))
	maps := w.postfixMaps()

	alias := maps[postfixVirtualAliasKey]
	for _, want := range []string{
		"abuse@k.example ops@outside.example\n",
		"admin@k.example admin@k.example\n",
		"@k.example @team.k.example\n",
	} {
		if !strings.Contains(alias, want) {
			t.Errorf("virtual_alias lacks %q:\n%s", want, alias)
		}
	}
	if strings.Count(alias, "abuse@k.example ") != 1 {
		t.Errorf("the role address is listed twice:\n%s", alias)
	}
	if strings.Index(alias, "admin@k.example admin@k.example") > strings.Index(alias, "@k.example @team.k.example") {
		t.Errorf("the domain-wide line comes before an address that keeps its mailbox:\n%s", alias)
	}
	if want := "/^([^@]+)@k\\.example$/ ${1}@k.example,${1}@team.k.example\n"; !strings.Contains(maps[postfixSenderLoginKey], want) {
		t.Errorf("sender_login lacks %q:\n%s", want, maps[postfixSenderLoginKey])
	}
}

// A mail tenant that cannot take the cluster's domain changes nothing: one
// with no mail domain here, the platform's own entry, or none at all.
func TestAMailTenantThatCannotTakeTheDomainChangesNothing(t *testing.T) {
	registry := map[string]string{kernelMailRegistryKey: "k.example", "user": "k.example", "team": "team.k.example"}
	for _, tenant := range []string{"", "nobody", kernelMailRegistryKey, "user"} {
		if got := kernelDomainMailTarget(registry, tenant, "k.example"); got != "" {
			t.Errorf("mail tenant %q delivers to %q", tenant, got)
		}
	}
	if got := kernelDomainMailTarget(registry, "team", "K.example"); got != "team.k.example" {
		t.Errorf("team delivers to %q", got)
	}
	w := newMailboxWorld(t, mailRegistry(registry), clusterConfig(map[string]string{clusterConfigMailTenantKey: "nobody"}))
	if alias := w.postfixMaps()[postfixVirtualAliasKey]; strings.Contains(alias, "@k.example @") {
		t.Errorf("a tenant with no mail domain got the cluster's:\n%s", alias)
	}
}

// Without the kernel realm's addresses the domain-wide line would take their
// mail, so a failed lookup writes nothing and leaves the maps as they were.
func TestTheMailTenantWaitsForTheKernelRealm(t *testing.T) {
	w := newMailboxWorld(t, mailRegistry(map[string]string{kernelMailRegistryKey: "k.example", "team": "team.k.example"}),
		clusterConfig(map[string]string{clusterConfigMailTenantKey: "team"}))
	// Keycloak cannot be asked: the operator's credential for it is gone.
	if err := w.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keycloakAdminSecret, Namespace: identityNamespace}}); err != nil {
		t.Fatal(err)
	}
	if err := w.tr.syncPostfixVirtualMailboxMaps(context.Background()); err == nil {
		t.Fatal("the maps were written without the kernel realm's addresses")
	}
}
