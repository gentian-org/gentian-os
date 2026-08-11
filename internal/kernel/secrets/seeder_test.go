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

package secrets_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// newFakeBao returns an httptest.Server that emulates the OpenBao KV v2 API
// just enough for the seeder unit tests.
func newFakeBao() *httptest.Server {
	data := map[string]map[string]string{}
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/secret/data/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			v, ok := data[key]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": v}})
		case http.MethodPost:
			var body struct {
				Data    map[string]string `json:"data"`
				Options struct {
					CAS *int `json:"cas"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.Options.CAS != nil && *body.Options.CAS == 0 {
				if _, exists := data[key]; exists {
					http.Error(w, `{"errors":["check-and-set parameter did not match the current version"]}`, http.StatusBadRequest)
					return
				}
			}
			data[key] = body.Data
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
}

func newClient(t *testing.T, addr string) *secrets.KVClient {
	t.Helper()
	c := secrets.NewKVClient(addr, "test-role", "")
	c.SetStaticToken("static")
	return c
}

func TestKVClientPutOnceIsIdempotent(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	c := newClient(t, srv.URL)

	ctx := context.Background()
	path := "gentian-os/tenants/t/apps/a/oidc"
	if err := c.PutOnce(ctx, path, map[string]string{"client-secret": "first"}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := c.PutOnce(ctx, path, map[string]string{"client-secret": "second"}); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := c.Get(ctx, path)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["client-secret"] != "first" {
		t.Fatalf("PutOnce overwrote: got %q, want %q", got["client-secret"], "first")
	}
}

func TestSeederSeedOIDCUpdatesIssuerPreservesSecret(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver("unit-test-master"))

	ctx := context.Background()
	first, err := s.SeedOIDC(ctx, "demo", "test-app",
		"https://id.example/realms/demo", "test-oidc-client")
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	second, err := s.SeedOIDC(ctx, "demo", "test-app",
		"https://id.example/auth/realms/demo", "test-oidc-client")
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.Issuer != "https://id.example/auth/realms/demo" {
		t.Fatalf("issuer not updated: %q", second.Issuer)
	}
	if first.ClientSecret != second.ClientSecret {
		t.Fatalf("client secret changed: %q → %q", first.ClientSecret, second.ClientSecret)
	}
}

func TestSeederSeedOIDCReturnsStoredSecretOnRepeat(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	d := secrets.NewDeriver("unit-test-master")
	s := secrets.NewSeeder(newClient(t, srv.URL), d)

	ctx := context.Background()
	first, err := s.SeedOIDC(ctx, "gtn-demo", "element",
		"https://id.example/realms/gtn-demo", "gtn-demo-element")
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	second, err := s.SeedOIDC(ctx, "gtn-demo", "element",
		"https://id.example/realms/gtn-demo", "gtn-demo-element")
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if first.ClientSecret != second.ClientSecret {
		t.Fatalf("client secret changed between reconciles: %q → %q", first.ClientSecret, second.ClientSecret)
	}
	if first.ClientSecret == "" || len(first.ClientSecret) != 40 {
		t.Fatalf("unexpected client secret %q", first.ClientSecret)
	}
}

func TestSeederSeedAppSecretWriteOnce(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver("unit-test-master"))

	ctx := context.Background()
	v1, err := s.SeedAppSecret(ctx, "gtn-demo", "element", "registration_shared_secret")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	v2, err := s.SeedAppSecret(ctx, "gtn-demo", "element", "registration_shared_secret")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if v1 != v2 || v1 == "" {
		t.Fatalf("not stable: %q vs %q", v1, v2)
	}
}

// TestDeriverDeterministicAcrossInstances verifies that two Derivers
// constructed from the same master produce identical output for the same
// (salt, info) — this is the property that lets uninstalling and
// reinstalling an app return identical credentials.
func TestDeriverDeterministicAcrossInstances(t *testing.T) {
	d1 := secrets.NewDeriver("master-X")
	d2 := secrets.NewDeriver("master-X")
	salt := secrets.CategoryPath("acme", "element", "oidc")
	a := d1.Derive(salt, "client-secret", 40)
	b := d2.Derive(salt, "client-secret", 40)
	if a != b {
		t.Fatalf("HKDF non-deterministic: %q vs %q", a, b)
	}
	if len(a) != 40 {
		t.Fatalf("wrong length: %d", len(a))
	}
}

// TestDeriverDifferentTenantsDifferentSecrets verifies that the tenant
// component of CategoryPath actually changes the derived value, so a single
// shared master is collision-free across tenants.
func TestDeriverDifferentTenantsDifferentSecrets(t *testing.T) {
	d := secrets.NewDeriver("master-X")
	a := d.Derive(secrets.CategoryPath("tenant-a", "element", "oidc"), "client-secret", 40)
	b := d.Derive(secrets.CategoryPath("tenant-b", "element", "oidc"), "client-secret", 40)
	if a == b {
		t.Fatalf("tenant salt did not diversify output")
	}
}

// TestDeriverKernelPathOmitsTenant documents that kernel-shared secrets do
// not include a tenant component.
func TestDeriverKernelPathOmitsTenant(t *testing.T) {
	got := secrets.KernelPath("internal", "master-password")
	want := "gentian-os/kernel/internal/master-password"
	if got != want {
		t.Fatalf("KernelPath mismatch: %q vs %q", got, want)
	}
}

func TestDeriverSaltDiversifiesOutput(t *testing.T) {
	d1 := secrets.NewDeriver("master-X", "salt-A")
	d2 := secrets.NewDeriver("master-X", "salt-B")
	salt := secrets.CategoryPath("acme", "element", "oidc")
	a := d1.Derive(salt, "client-secret", 40)
	b := d2.Derive(salt, "client-secret", 40)
	if a == b {
		t.Fatalf("per-cluster salt did not diversify output")
	}
}

func TestSeederSeedCacheRecordsUser(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver("unit-test-master"))

	ctx := context.Background()
	first, err := s.SeedCache(ctx, "demo", "test-app", secrets.CacheCreds{
		Host: "redis.kernel.svc", Port: "6379", User: "demo-test-app",
	})
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if first.User != "demo-test-app" {
		t.Fatalf("user not recorded: %q", first.User)
	}
	if first.Password == "" {
		t.Fatal("password not derived")
	}

	// A reconcile that does not supply the user must not erase the stored one,
	// and must keep returning the same derived password.
	second, err := s.SeedCache(ctx, "demo", "test-app", secrets.CacheCreds{
		Host: "redis.kernel.svc", Port: "6379",
	})
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.User != "demo-test-app" {
		t.Fatalf("stored user clobbered: %q", second.User)
	}
	if second.Password != first.Password {
		t.Fatalf("password changed: %q → %q", first.Password, second.Password)
	}
}

func TestSeederSeedCacheOmitsUserWhenEngineHasNone(t *testing.T) {
	srv := newFakeBao()
	defer srv.Close()
	s := secrets.NewSeeder(newClient(t, srv.URL), secrets.NewDeriver("unit-test-master"))

	// memcached has no per-app ACL user; the record must not carry an empty one.
	got, err := s.SeedCache(context.Background(), "demo", "memcached-app", secrets.CacheCreds{
		Host: "memcached.kernel.svc", Port: "11211",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got.User != "" {
		t.Fatalf("unexpected user: %q", got.User)
	}
}
