/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
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
	}, "k.example", ""))
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
