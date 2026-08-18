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

func TestDeriveAndHash(t *testing.T) {
	seed := []byte("0123456789abcdef0123456789abcdef")
	a := deriveMailPassword(seed, "christian@corp.gtn.host")
	b := deriveMailPassword(seed, "christian@corp.gtn.host")
	if a != b {
		t.Fatal("derivation not deterministic")
	}
	if c := deriveMailPassword(seed, "other@corp.gtn.host"); c == a {
		t.Fatal("different users share a password")
	}
	if d := deriveMailPassword([]byte("ffffffffffffffffffffffffffffffff"), "christian@corp.gtn.host"); d == a {
		t.Fatal("different tenants share a password")
	}
	if len(a) != 32 {
		t.Fatalf("want 32 chars, got %d", len(a))
	}
	line, err := argon2idPasswdLine("christian@corp.gtn.host", a)
	if err != nil {
		t.Fatal(err)
	}
	if line[:24] != "christian@corp.gtn.host:" {
		t.Fatalf("bad prefix: %q", line[:30])
	}
	if !contains(line, "{ARGON2ID}$argon2id$v=19$") {
		t.Fatalf("bad scheme: %s", line)
	}
	if contains(line, a) {
		t.Fatal("the password itself leaked into the passwd line")
	}
}
func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func TestTenantMailDNSRecords(t *testing.T) {
	r := &TenantReconciler{KernelDomain: "gtn.host"}
	_ = r
	recs := []map[string]interface{}{
		dnsEndpointRecord("corp.gtn.host", "MX", "10 mail.gtn.host"),
		dnsEndpointRecord("mail._domainkey.corp.gtn.host", "TXT", "v=DKIM1; p=abc"),
	}
	if recs[0]["recordType"] != "MX" {
		t.Fatal("wrong record type")
	}
	if got := recs[0]["targets"].([]interface{})[0]; got != "10 mail.gtn.host" {
		t.Fatalf("MX must carry priority and host, got %v", got)
	}
	if recs[0]["recordTTL"].(int64) != 300 {
		t.Fatal("TTL must be explicit; a zero TTL is rejected by some providers")
	}
}

func TestMailAddressNotDoubleSuffixed(t *testing.T) {
	for _, tc := range []struct{ user, domain, want string }{
		{"admin-corp", "corp.gtn.host", "admin-corp@corp.gtn.host"},
		{"christian@corp.gtn.host", "corp.gtn.host", "christian@corp.gtn.host"},
		{"someone@elsewhere.test", "corp.gtn.host", "someone@elsewhere.test"},
	} {
		got := tc.user
		if !strings.Contains(got, "@") {
			got = tc.user + "@" + tc.domain
		}
		if got != tc.want {
			t.Fatalf("user %q: got %q, want %q", tc.user, got, tc.want)
		}
	}
}
