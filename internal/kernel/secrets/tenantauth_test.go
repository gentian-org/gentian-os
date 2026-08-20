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
	"net/http"
	"net/http/httptest"
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

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme"); err != nil {
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

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme"); err != nil {
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

	if err := testTenantAuth(srv).EnsureMount(context.Background(), "acme", "https://kc/realms/acme"); err != nil {
		t.Fatalf("EnsureMount should tolerate 'already in use': %v", err)
	}
}
