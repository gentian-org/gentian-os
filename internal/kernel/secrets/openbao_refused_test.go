/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// forgetfulVault issues a new token at every login and honors only the
// latest; revoke makes it refuse every token it issued so far, as a vault
// does after a restart. denyAll answers 403 to every token, as a policy that
// does not grant the path does.
type forgetfulVault struct {
	mu      sync.Mutex
	logins  int
	valid   string
	denyAll bool
}

func (v *forgetfulVault) revoke() { v.mu.Lock(); v.valid = ""; v.mu.Unlock() }

func (v *forgetfulVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.URL.Path == "/v1/auth/kubernetes/login" {
		v.logins++
		v.valid = fmt.Sprintf("token-%d", v.logins)
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": v.valid, "lease_duration": 3600}})
		return
	}
	if v.denyAll || r.Header.Get("X-Vault-Token") != v.valid {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"k": "v"}}})
}

func forgetfulClient(t *testing.T, v *forgetfulVault) *KVClient {
	t.Helper()
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewKVClient(srv.URL, "role", jwt)
}

// A token the vault no longer honors is dropped at the first refusal and the
// request made again with a fresh login, rather than refused until the
// cached token expires.
func TestARefusedTokenIsDroppedAndTheRequestMadeAgain(t *testing.T) {
	ctx := context.Background()
	v := &forgetfulVault{}
	c := forgetfulClient(t, v)
	if _, err := c.Get(ctx, "gentian-os/x"); err != nil {
		t.Fatal(err)
	}
	v.revoke()
	for name, call := range map[string]func() error{
		"Get":    func() error { _, err := c.Get(ctx, "gentian-os/x"); return err },
		"Exists": func() error { _, err := c.Exists(ctx, "gentian-os/x"); return err },
		"Put":    func() error { return c.Put(ctx, "gentian-os/x", map[string]string{"k": "v"}) },
		"Request": func() error {
			status, _, err := c.Request(ctx, http.MethodGet, "secret/data/gentian-os/x", nil)
			return requestErr(status, err)
		},
	} {
		before := v.logins
		if err := call(); err != nil {
			t.Errorf("%s after the vault forgot the token: %v", name, err)
		}
		if v.logins != before+1 {
			t.Errorf("%s logged in %d times, want once", name, v.logins-before)
		}
		v.revoke()
	}
}

// A refusal that is the policy's and not the token's is answered after one
// more try, not retried for ever.
func TestARefusalThatPersistsIsReturnedAfterOneRetry(t *testing.T) {
	v := &forgetfulVault{denyAll: true}
	c := forgetfulClient(t, v)
	if _, err := c.Get(context.Background(), "gentian-os/x"); err == nil {
		t.Fatal("a path the policy denies was read")
	}
	if v.logins != 2 {
		t.Fatalf("logged in %d times, want 2", v.logins)
	}
}

// A static token has nothing to log in with in its place, so it is kept.
func TestAStaticTokenIsNotDropped(t *testing.T) {
	v := &forgetfulVault{denyAll: true}
	c := forgetfulClient(t, v)
	c.SetStaticToken("static")
	if _, err := c.Get(context.Background(), "gentian-os/x"); err == nil {
		t.Fatal("a refused static token read the path")
	}
	if v.logins != 0 {
		t.Fatalf("logged in %d times with a static token", v.logins)
	}
}

func requestErr(status int, err error) error {
	if err == nil && status != http.StatusOK {
		return fmt.Errorf("HTTP %d", status)
	}
	return err
}
