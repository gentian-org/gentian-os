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

// Package directortest holds the fixtures the director's contract tests share:
// a bare git remote with a tenant in it, and a static-key OIDC issuer.
package directortest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Cluster is the cluster name every fixture repository uses; KernelDomain is
// what its Cluster claim declares.
const (
	Cluster      = "demo-cluster"
	KernelDomain = "k.example"
)

// TenantYAML is a minimal tenant manifest.
func TenantYAML(name string) string {
	return `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: ` + name + `
spec:
  displayName: ` + name + `
  apps:
  - profile: nextcloud
`
}

// Remote creates a bare repository holding the given tenants and returns its path.
func Remote(t testing.TB, tenants ...string) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	Git(t, "", "init", "--bare", "--initial-branch=main", remote)
	seed := t.TempDir()
	Git(t, "", "clone", remote, seed)
	claims := filepath.Join(seed, "clusters", Cluster, "kernel", "claims")
	if err := os.MkdirAll(claims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claims, "cluster.yaml"), []byte("apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: "+Cluster+"\nspec:\n  kernelDomain: "+KernelDomain+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range tenants {
		dir := filepath.Join(seed, "clusters", Cluster, "tenants", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tenant.yaml"), []byte(TenantYAML(name)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, seed, "add", "-A")
	Git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	Git(t, seed, "push", "origin", "HEAD:main")
	return remote
}

// Clone returns a fresh checkout of remote — what one director replica owns.
func Clone(t testing.TB, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checkout")
	Git(t, "", "clone", remote, dir)
	return dir
}

// Git runs git and fails the test on error, returning trimmed output.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// RemoteFile reads a file at the tip of the remote's main branch.
func RemoteFile(t testing.TB, remote, path string) string {
	t.Helper()
	return Git(t, "", "--git-dir", remote, "show", "main:"+path)
}

// TenantPath is the repository-relative path of a tenant manifest.
func TenantPath(tenant string) string {
	return "clusters/" + Cluster + "/tenants/" + tenant + "/tenant.yaml"
}

// Issuer is a Keycloak-shaped token issuer with static keys.
type Issuer struct {
	*httptest.Server
	key    *rsa.PrivateKey
	kid    string
	Served int // key-set requests answered
}

// NewIssuer starts an issuer that serves its key set for any realm in realms.
func NewIssuer(t testing.TB, realms ...string) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{key: key, kid: "test-key-1"}
	known := map[string]bool{}
	for _, r := range realms {
		known[r] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /realms/{realm}/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		if !known[r.PathValue("realm")] {
			http.NotFound(w, r)
			return
		}
		is.Served++
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig"},
		}})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

// Claims describes a token to mint. Zero values get sensible defaults.
type Claims struct {
	Realm    string
	Subject  string
	Audience string
	Issuer   string // overrides the realm-derived issuer
	Expiry   time.Time
	Type     string // typ claim; default "Bearer"
	Name     string
	Email    string
	Key      *rsa.PrivateKey // sign with a key the issuer does not publish
	KeyID    string
}

// Token mints a signed token.
func (is *Issuer) Token(t testing.TB, c Claims) string {
	t.Helper()
	key, kid := is.key, is.kid
	if c.Key != nil {
		key = c.Key
	}
	if c.KeyID != "" {
		kid = c.KeyID
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	iss := c.Issuer
	if iss == "" {
		iss = is.URL + "/realms/" + c.Realm
	}
	exp := c.Expiry
	if exp.IsZero() {
		exp = time.Now().Add(5 * time.Minute)
	}
	typ := c.Type
	if typ == "" {
		typ = "Bearer"
	}
	std := jwt.Claims{
		Issuer: iss, Subject: c.Subject, Audience: jwt.Audience{c.Audience},
		Expiry: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Second)),
	}
	extra := map[string]any{"typ": typ, "sid": "sid-" + c.Subject, "azp": "gentian-console", "name": c.Name, "email": c.Email}
	raw, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// OtherKey returns a key the issuer does not publish.
func OtherKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Statement signs payload as the store would: a compact JWS, EdDSA, with the
// key id in the protected header.
func Statement(t testing.TB, key ed25519.PrivateKey, kid string, payload any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
