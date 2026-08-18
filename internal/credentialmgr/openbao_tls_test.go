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
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenBao serves a self-signed certificate on this platform. The client used to
// be built with a bare http.Client, which verifies against the system roots, so
// every exchange failed at TLS — and the handler reported that as 401, telling
// an administrator their group membership was wrong.
//
// httptest.NewTLSServer serves exactly that shape: a certificate signed by
// nobody the system trusts.
func newSelfSignedOpenBao(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.tok","token_policies":["cluster-admin"],"metadata":{"username":"admin"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serverCAPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// The regression: no CA, self-signed upstream, and the failure must be
// reported as unreachable rather than as a rejected caller.
func TestExchange_SelfSignedWithoutCA_IsUpstreamNotAuthz(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"cluster-admin-jwt"}, nil, false)

	_, err := b.ExchangeToken(context.Background(), "a.jwt.token")
	if err == nil {
		t.Fatal("expected the exchange to fail against an untrusted certificate")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("a TLS failure must be reported as unreachable, not as a rejected token; got: %v", err)
	}
}

// With the CA supplied — what loadBaoCA reads out of openbao-tls — the same
// exchange succeeds.
func TestExchange_SelfSignedWithCA_Succeeds(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"cluster-admin-jwt"}, serverCAPEM(t, srv), false)

	id, err := b.ExchangeToken(context.Background(), "a.jwt.token")
	if err != nil {
		t.Fatalf("expected the exchange to succeed once the CA is trusted, got: %v", err)
	}
	if id.Token != "s.tok" {
		t.Errorf("token = %q, want s.tok", id.Token)
	}
	if len(id.Policies) != 1 || id.Policies[0] != "cluster-admin" {
		t.Errorf("policies = %v, want [cluster-admin]", id.Policies)
	}
}

// The escape hatch, for a cluster whose CA cannot be reached at all.
func TestExchange_SkipVerify_Succeeds(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"cluster-admin-jwt"}, nil, true)

	if _, err := b.ExchangeToken(context.Background(), "a.jwt.token"); err != nil {
		t.Fatalf("expected skip-verify to connect, got: %v", err)
	}
}

// Garbage in the CA Secret must not silently disable TLS. Falling back to the
// system roots fails closed; falling back to InsecureSkipVerify would not.
func TestExchange_InvalidCAPEM_StillVerifies(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"cluster-admin-jwt"}, []byte("not a certificate"), false)

	_, err := b.ExchangeToken(context.Background(), "a.jwt.token")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("an unparseable CA must leave verification on; got: %v", err)
	}
}

// A refusal by OpenBao is still a refusal — the new sentinel must not swallow
// the case it was added to distinguish.
func TestExchange_RoleRefusal_IsNotUpstream(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"cluster-admin-jwt"}, serverCAPEM(t, srv), false)

	_, err := b.ExchangeToken(context.Background(), "a.jwt.token")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if errors.Is(err, ErrUpstream) {
		t.Fatalf("a role refusal is not an upstream failure; got: %v", err)
	}
}

// A rejected write must carry OpenBao's own explanation. "the caller's policy
// may not permit this" was a guess that happened to be wrong, and it sent an
// operator to audit a policy that already granted the path.
func TestWrite_RejectionCarriesOpenBaosAnswer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`))
	}))
	defer srv.Close()
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"r"}, serverCAPEM(t, srv), false)

	err := b.Write(context.Background(), "s.tok", "gentian-os/kernel/mail/postfix", map[string]string{"k": "v"}, "admin")
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	for _, want := range []string{"403", "permission denied", "gentian-os/kernel/mail/postfix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q; got: %v", want, err)
		}
	}
	if errors.Is(err, ErrUpstream) {
		t.Error("a rejection is not an upstream failure")
	}
}

// And an unreachable OpenBao on the write path must not read as a policy problem.
func TestWrite_UnreachableIsUpstream(t *testing.T) {
	b := NewOpenBao("https://127.0.0.1:1", "secret", "oidc", []string{"r"}, nil, false)
	err := b.Write(context.Background(), "s.tok", "gentian-os/kernel/mail/postfix", map[string]string{"k": "v"}, "admin")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("expected an upstream failure; got: %v", err)
	}
}

// The write must not carry a check-and-set.
//
// It sent {"options":{"cas":null}}. cas 0 means "only if absent", and null is
// not "no opinion" — so every write to a path that already had a version was
// rejected. The installer seeds this mount at bootstrap, so every credential
// supplied afterwards is an update of an existing path: the one operation this
// service exists for was the one it could never perform.
func TestWrite_SendsNoCheckAndSet(t *testing.T) {
	var got map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"version":2}}`))
	}))
	defer srv.Close()
	b := NewOpenBao(srv.URL, "secret", "oidc", []string{"r"}, serverCAPEM(t, srv), false)

	if err := b.Write(context.Background(), "s.tok",
		"gentian-os/kernel/mail/postfix", map[string]string{"relay_username": "u"}, ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, present := got["options"]; present {
		t.Errorf("the write must send no options block; got %v", got["options"])
	}
	data, _ := got["data"].(map[string]any)
	if data["relay_username"] != "u" {
		t.Errorf("data = %v, want the supplied fields", got["data"])
	}
}
