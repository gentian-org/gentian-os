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

package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testTenantAuth returns a TenantAuth pointed at srv with a token already
// cached, so no Kubernetes login is attempted.
func testTenantAuth(srv *httptest.Server) *TenantAuth {
	return NewTenantAuth(&KVClient{
		addr:     srv.URL,
		http:     srv.Client(),
		token:    "test-token",
		tokenExp: time.Now().Add(time.Hour),
	})
}

// EnsureMount has to create a mount that does not exist yet, and OpenBao does
// not say "absent" the way one would guess: reading a mount that is not there
// answers 400 "No auth engine at <path>/", not 404.
//
// Keying the create on 404 therefore never fired. The mount was never enabled,
// and the config write that follows failed with 404 "route entry not found" —
// which reads as a wrong config path rather than a missing mount, and cost a
// long detour through permissions before the cause showed itself.
func TestEnsureMountCreatesWhenReadSaysBadRequest(t *testing.T) {
	var enabled bool
	var configured bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/auth/oidc-acme":
			if enabled {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"type":"jwt"}`))
				return
			}
			// What OpenBao actually answers for a mount that is not there.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["No auth engine at oidc-acme/"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sys/auth/oidc-acme":
			enabled = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/oidc-acme/config":
			if !enabled {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":["no handler for route \"auth/oidc-acme/config\". route entry not found."]}`))
				return
			}
			configured = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme", ""); err != nil {
		t.Fatalf("EnsureMount: %v", err)
	}
	if !enabled {
		t.Error("mount was never enabled; the 400 read was treated as 'exists'")
	}
	if !configured {
		t.Error("mount was never configured")
	}
}

// An existing mount must not be re-enabled: OpenBao refuses with 400 "path is
// already in use", and treating that as fatal would fail every reconcile after
// the first.
func TestEnsureMountSkipsCreateWhenPresent(t *testing.T) {
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/auth/oidc-acme":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"jwt"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sys/auth/oidc-acme":
			creates++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["path is already in use at oidc-acme/"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/oidc-acme/config":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme", ""); err != nil {
		t.Fatalf("EnsureMount: %v", err)
	}
	if creates != 0 {
		t.Errorf("tried to enable a mount that already exists (%d attempts)", creates)
	}
}

// And when the read is wrong about absence — a permission gap, or a mount
// created between the two calls — "already in use" is the mount existing, not a
// failure.
func TestEnsureMountToleratesRaceOnCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/auth/oidc-acme":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["No auth engine at oidc-acme/"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sys/auth/oidc-acme":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["path is already in use at oidc-acme/"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/oidc-acme/config":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme", ""); err != nil {
		t.Fatalf("EnsureMount should tolerate 'already in use': %v", err)
	}
}

// A cluster whose Keycloak serves a certificate OpenBao does not trust refuses
// the config write with "error checking oidc discovery URL" - it fetches the
// discovery document itself to validate the write. The retry pinned to the
// cluster's own chain is what makes the mount configurable there, and without
// it every reconcile of every tenant fails on a message that names the URL and
// not the trust problem.
func TestEnsureMountRetriesWithPinnedCA(t *testing.T) {
	var configBodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/auth/oidc-acme":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"jwt"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/oidc-acme/config":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			configBodies = append(configBodies, body)
			if _, pinned := body["oidc_discovery_ca_pem"]; !pinned {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":["error checking oidc discovery URL"]}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme", "PEM"); err != nil {
		t.Fatalf("EnsureMount: %v", err)
	}
	if len(configBodies) != 2 {
		t.Fatalf("expected an unpinned attempt then a pinned one, got %d writes", len(configBodies))
	}
	// Unpinned first, so a cluster with a publicly trusted certificate is never
	// pinned to a chain it would have to be re-pinned away from on renewal.
	if _, pinned := configBodies[0]["oidc_discovery_ca_pem"]; pinned {
		t.Error("first attempt pinned a CA; it must try the system pool first")
	}
	if got := configBodies[1]["oidc_discovery_ca_pem"]; got != "PEM" {
		t.Errorf("retry did not pin the supplied chain: %v", got)
	}
}

// With no chain to pin, the original refusal is what the operator must see.
// Swallowing it, or reporting a retry that never happened, hides the reason.
func TestEnsureMountWithoutCAReportsOriginalFailure(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/auth/oidc-acme":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"jwt"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/oidc-acme/config":
			writes++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["error checking oidc discovery URL"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme", "")
	if err == nil {
		t.Fatal("EnsureMount succeeded on a refused config write")
	}
	if !strings.Contains(err.Error(), "error checking oidc discovery URL") {
		t.Errorf("error dropped what OpenBao objected to: %v", err)
	}
	if writes != 1 {
		t.Errorf("expected exactly one attempt with no CA to pin, got %d", writes)
	}
}
