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

package credentialmgr

import (
	"strings"
	"testing"
)

const testIdentity = "AGE-SECRET-KEY-1QZS05PTS7RRWH8GJMHW3JDVVQEVCMJH8AS9EEXAMPLEKEYVALUEXX"

// The path is derived from the verified claim, never from the request.
//
// This is the whole authorisation model for escrow. A tenant name taken from
// the body would let any workspace administrator write a key into another
// workspace's subtree — and OpenBao would allow it for whichever of the two the
// caller's policy happened to cover, so the failure would be silent for the
// pair that mattered.
func TestBackupIdentityPathComesFromTheClaim(t *testing.T) {
	if got, want := BackupIdentityPath("acme"), "gentian-os/tenants/acme/backup/identity"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	s, _ := newServerAsTenant(t, "acme")
	// A body naming a different tenant. There is no field for it, which is the
	// point — this asserts that adding one later would be a change in behaviour
	// somebody has to make deliberately.
	w := do(t, s, "PUT", "/v1/backup-identity",
		`{"identity":"`+testIdentity+`","tenant":"globex"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tenants/acme/backup/identity") {
		t.Errorf("wrote somewhere other than the caller's own subtree: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "globex") {
		t.Errorf("a tenant named in the body reached the path: %s", w.Body.String())
	}
}

// A caller with no tenant has no subtree to escrow into, and must not fall back
// to one. A cluster administrator's backup key lives in the recovery kit and at
// the kernel path, which is a different arrangement with a different policy.
func TestBackupIdentityRefusesACallerWithNoTenant(t *testing.T) {
	s, _ := newServer(t) // cluster-admin, no tenant claim
	w := do(t, s, "PUT", "/v1/backup-identity", `{"identity":"`+testIdentity+`"}`)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
	}
}

// The public half pasted into the private half's field is the mistake this
// catches. Escrowing a recipient would store something that looks like a key,
// reads back cleanly, and opens nothing — discovered at a restore, years later.
func TestBackupIdentityRefusesWhatIsNotAPrivateKey(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme")
	for name, body := range map[string]string{
		"a public key":  `{"identity":"age17lr9cmnutfg66r92rwc20umdz82sgx3wq86c5lmht8d7sm8dlqpqr3d4zw"}`,
		"empty":         `{"identity":""}`,
		"a whole file":  `{"identity":"` + strings.Repeat("x", 300) + `"}`,
		"not JSON":      `not json at all`,
		"missing field": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := do(t, s, "PUT", "/v1/backup-identity", body); w.Code != 400 {
				t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
			}
		})
	}
}

// Stored, not echoed. An endpoint that returned the key would put it in every
// proxy access log between here and the browser, which is a copy nobody chose
// to make and nobody can delete.
func TestBackupIdentityDoesNotEchoTheKey(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme")
	w := do(t, s, "PUT", "/v1/backup-identity", `{"identity":"`+testIdentity+`"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), testIdentity) {
		t.Errorf("the response carried the key back:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"stored":true`) {
		t.Errorf("no confirmation that it was stored: %s", w.Body.String())
	}
}
