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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		_, _ = fmt.Fprintf(w, `{"auth":{"client_token":%q,"lease_duration":3600}}`, v.valid)
		return
	}
	if v.denyAll || r.Header.Get("X-Vault-Token") != v.valid {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	if strings.Contains(r.URL.Path, "/metadata/") && r.Method == http.MethodGet {
		_, _ = w.Write([]byte(`{"data":{"current_version":1,"custom_metadata":{"set_by":"alice"}}}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func forgetfulCustodian(t *testing.T, v *forgetfulVault) *OpenBao {
	t.Helper()
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	return asCustodian(t, NewOpenBao(srv.URL, "secret", "gentian-os-custodian", nil, false))
}

// A token the vault no longer honors is dropped at the first refusal and the
// call made again with a fresh login, rather than refused until the cached
// token expires.
func TestARefusedTokenIsDroppedAndTheCallMadeAgain(t *testing.T) {
	ctx := context.Background()
	v := &forgetfulVault{}
	b := forgetfulCustodian(t, v)
	if _, err := b.Token(ctx); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(string) error{
		"Write": func(token string) error {
			return b.Write(ctx, token, "gentian-os/kernel/x", map[string]string{"k": "v"}, "alice")
		},
		"Metadata": func(token string) error {
			md, err := b.Metadata(ctx, token, "gentian-os/kernel/x")
			if err == nil && md.SetBy != "alice" {
				return fmt.Errorf("metadata = %+v", md)
			}
			return err
		},
	} {
		v.revoke()
		before := v.logins
		if err := b.WithToken(ctx, call); err != nil {
			t.Errorf("%s after the vault forgot the token: %v", name, err)
		}
		if v.logins != before+1 {
			t.Errorf("%s logged in %d times, want once", name, v.logins-before)
		}
	}
}

// A refusal that is the policy's and not the token's is answered after one
// more try, as the vault worded it.
func TestARefusalThatPersistsIsReturnedAfterOneRetry(t *testing.T) {
	v := &forgetfulVault{denyAll: true}
	b := forgetfulCustodian(t, v)
	err := b.WithToken(context.Background(), func(token string) error {
		return b.Write(context.Background(), token, "gentian-os/kernel/x", map[string]string{"k": "v"}, "alice")
	})
	if !errors.Is(err, ErrTokenRefused) || !strings.HasPrefix(err.Error(), "openbao rejected the write") {
		t.Fatalf("err = %v", err)
	}
	if v.logins != 2 {
		t.Fatalf("logged in %d times, want 2", v.logins)
	}
}
