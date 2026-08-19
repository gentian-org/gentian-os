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
