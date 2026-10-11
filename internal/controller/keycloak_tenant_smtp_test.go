/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

func TestTenantSMTPJobName(t *testing.T) {
	got := tenantSMTPJobName("demo")
	want := "keycloak-tenant-smtp-demo"
	if got != want {
		t.Fatalf("tenantSMTPJobName = %q, want %q", got, want)
	}
}

// The gate reads smtp_configure, not the Secret's existence.
//
// The Secret is built by an ExternalSecret that renders the cluster's mail
// settings whether or not the relay credential has been supplied, so it exists
// on every external-mode cluster from the moment the kernel syncs. Gating on
// presence would send every tenant into a configure Job that exits 1 on its own
// SMTP_CONFIGURE check — and, before the ExternalSecret existed, gating on
// presence is what let a cluster with no credential skip the step in silence.
func TestSMTPCredentialsUsable(t *testing.T) {
	b := func(m map[string]string) map[string][]byte {
		out := map[string][]byte{}
		for k, v := range m {
			out[k] = []byte(v)
		}
		return out
	}

	cases := []struct {
		name string
		data map[string][]byte
		want bool
	}{
		{"nothing synced yet", nil, false},
		{"credential supplied", b(map[string]string{"smtp_configure": "true", "smtp_user": "relay@example.org"}), true},
		{"settings rendered, credential not supplied", b(map[string]string{"smtp_configure": "false", "smtp_host": "smtp.example.org"}), false},
		{"field absent", b(map[string]string{"smtp_host": "smtp.example.org"}), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := smtpCredentialsUsable(tc.data); got != tc.want {
				t.Fatalf("smtpCredentialsUsable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildTenantSMTPConfigureScriptRealmPlaceholderCount(t *testing.T) {
	script := buildTenantSMTPConfigureScript(`"demo"`)
	for _, want := range []string{`REALM="demo"`, "SMTP_HOST", "smtpServer = $smtp"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}
