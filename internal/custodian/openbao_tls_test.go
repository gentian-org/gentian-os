/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// asCustodian points a client at a ServiceAccount token on disk, which is what
// the custodian presents when it logs in as itself.
func asCustodian(t *testing.T, b *OpenBao) *OpenBao {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("sa.jwt.token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.ServiceAccountTokenPath = path
	return b
}

// The regression: no CA, self-signed upstream, and the failure must be
// reported as unreachable rather than as a refused login.
func TestLogin_SelfSignedWithoutCA_IsUpstream(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", nil, false))

	_, err := b.Token(context.Background())
	if err == nil {
		t.Fatal("expected the login to fail against an untrusted certificate")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("a TLS failure must be reported as unreachable, not as a refused login; got: %v", err)
	}
}

// With the CA supplied — what loadBaoCA reads out of openbao-tls — the same
// login succeeds, and the token is kept rather than fetched for every request.
func TestLogin_SelfSignedWithCA_SucceedsAndIsKept(t *testing.T) {
	logins := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/kubernetes/login" {
			t.Errorf("the custodian logged in at %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"role":"gentian-os-custodian"`) || !strings.Contains(string(body), `"jwt":"sa.jwt.token"`) {
			t.Errorf("login body = %s", body)
		}
		logins++
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.tok","lease_duration":3600}}`))
	}))
	defer srv.Close()
	b := asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", serverCAPEM(t, srv), false))

	for i := 0; i < 3; i++ {
		tok, err := b.Token(context.Background())
		if err != nil || tok != "s.tok" {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	if logins != 1 {
		t.Fatalf("logged in %d times for three requests", logins)
	}
	// Told its token is no good, it logs in again.
	b.forget()
	if _, err := b.Token(context.Background()); err != nil || logins != 2 {
		t.Fatalf("after forgetting: logins = %d, err = %v", logins, err)
	}
}

// The escape hatch, for a cluster whose CA cannot be reached at all.
func TestLogin_SkipVerify_Succeeds(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", nil, true))

	if _, err := b.Token(context.Background()); err != nil {
		t.Fatalf("expected skip-verify to connect, got: %v", err)
	}
}

// Garbage in the CA Secret must not silently disable TLS. Falling back to the
// system roots fails closed; falling back to InsecureSkipVerify would not.
func TestLogin_InvalidCAPEM_StillVerifies(t *testing.T) {
	srv := newSelfSignedOpenBao(t)
	b := asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", []byte("not a certificate"), false))

	_, err := b.Token(context.Background())
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("an unparseable CA must leave verification on; got: %v", err)
	}
}

// A vault that refuses the custodian's login is a fault of the installation:
// it is not an unreachable vault, and what the vault said stays out of the
// error a caller could be shown.
func TestLogin_Refused_IsTheInstallationsFault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":["role \"gentian-os-custodian\" could not be found; policy custodian-write"]}`))
	}))
	defer srv.Close()
	b := asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", serverCAPEM(t, srv), false))

	_, err := b.Token(context.Background())
	if !errors.Is(err, ErrVaultLogin) || errors.Is(err, ErrUpstream) {
		t.Fatalf("a refused login = %v", err)
	}
	if strings.Contains(err.Error(), "custodian-write") {
		t.Fatalf("the vault's own words reached the error: %v", err)
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
	b := NewOpenBao(srv.URL, "secret", "gentian-os-custodian", serverCAPEM(t, srv), false)

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
	b := NewOpenBao("https://127.0.0.1:1", "secret", "gentian-os-custodian", nil, false)
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
	b := NewOpenBao(srv.URL, "secret", "gentian-os-custodian", serverCAPEM(t, srv), false)

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
