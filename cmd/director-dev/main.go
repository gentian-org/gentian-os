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
// Command director-dev runs the real director API on a laptop, with stand-ins
// for everything it depends on, so a UI can be built against it before there is
// a cluster.
//
// The API, the token verification, the authorization questions and the git
// writes are the production code. What is faked is only what they talk to:
//
//   - Keycloak is a local issuer with a throwaway key. GET /dev/token mints an
//     access token for any of the fixture people.
//   - OpenFGA is a table stating what model v1 answers for those people — or a
//     real OpenFGA, when OPENFGA_API_URL, OPENFGA_STORE_ID and OPENFGA_MODEL_ID
//     are set.
//   - gentian-deployments is a bare repository in a temporary directory, seeded
//     with two tenants. Inspect it with git: the commits are the real ones.
//   - The App Store is a throwaway signing key. GET /dev/statement signs an
//     entitlement statement.
//
// It is not shipped in any image and must never be: /dev/token makes anyone
// anybody.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/membership"
)

const (
	audience = "gentian-director"
	cluster  = "dev-cluster" // the default; -cluster overrides
)

// person is a fixture account. The cast is that of authz/model/v1's tests.
type person struct {
	Realm string `json:"realm"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

var people = map[string]person{
	"alice": {"kernel", "Alice Admin", "platform administrator; reaches demo through operated_by, not solo"},
	"tom":   {"demo", "Tom Tenant", "administrator of tenant demo"},
	"mia":   {"demo", "Mia Member", "member of tenant demo"},
	"tina":  {"solo", "Tina Solo", "administrator of tenant solo, which administers itself"},
	"olaf":  {"other", "Olaf Other", "member of another tenant; sees nothing here"},
}

// facts is what model v1 answers for the fixture. make test-director-contract
// is what shows this table and the model agree.
var facts = map[string]bool{
	"user:tom can_install_app tenant:demo":         true,
	"user:tom can_view tenant:demo":                true,
	"user:mia can_view tenant:demo":                true,
	"user:alice can_install_app tenant:demo":       true,
	"user:alice can_view tenant:demo":              true,
	"user:tina can_install_app tenant:solo":        true,
	"user:tina can_view tenant:solo":               true,
	"user:alice can_audit cluster:dev-cluster":     true,
	"user:alice can_configure cluster:dev-cluster": true,
}

// decisions answers from the table, and for entitlements from the tuples the
// director itself wrote, evaluating grant_valid as the model does.
type decisions struct {
	log    *slog.Logger
	tuples map[string]authz.Tuple
}

func key(t authz.Tuple) string { return t.User + " " + t.Relation + " " + t.Object }

func (d *decisions) Check(_ context.Context, requestID, user, relation, object string) (bool, error) {
	allowed := facts[user+" "+relation+" "+object]
	if relation == "can_install" {
		if t, ok := d.tuples[key(authz.Tuple{User: user, Relation: "entitled", Object: object})]; ok && t.Condition != nil {
			until, err := time.Parse(time.RFC3339, fmt.Sprint(t.Condition.Context["expires_at"]))
			allowed = err == nil && time.Now().Before(until)
		}
	}
	d.log.Info("authz decision", "request_id", requestID, "user", user, "relation", relation, "object", object, "allowed", allowed)
	return allowed, nil
}

func (d *decisions) Read(_ context.Context, f authz.Tuple) ([]authz.Tuple, error) {
	if t, ok := d.tuples[key(f)]; ok {
		return []authz.Tuple{t}, nil
	}
	return nil, nil
}

func (d *decisions) Write(_ context.Context, writes, deletes []authz.Tuple) error {
	for _, t := range deletes {
		delete(d.tuples, key(t))
	}
	for _, t := range writes {
		d.tuples[key(t)] = t
	}
	return nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to serve on")
	origins := flag.String("cors", "http://localhost:5173", "comma-separated origins allowed to call the API from a browser")
	public := flag.String("url", "", "URL the API is reached at, when it differs from http://<listen> (a published container port)")
	entitlements := flag.Bool("entitlements", false, "require an entitlement to install, as a cluster with a store does")
	storeKeys := flag.String("store-keys", "", "pin a store's keys, as DIRECTOR_STORE_KEYS does (id=base64,…); default: a throwaway key that /dev/statement signs with")
	clusterID := flag.String("cluster", cluster, "the cluster id statements must be addressed to")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if *public == "" {
		*public = "http://" + *listen
	}
	if err := run(log, *listen, strings.TrimRight(*public, "/"), strings.Split(*origins, ","), *entitlements, *storeKeys, *clusterID); err != nil {
		log.Error("director-dev stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, base string, origins []string, enforce bool, storeKeys, cluster string) error {
	work, err := os.MkdirTemp("", "director-dev-")
	if err != nil {
		return err
	}
	remote, err := seed(work, cluster)
	if err != nil {
		return err
	}

	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	storePub, storeKey, _ := ed25519.GenerateKey(rand.Reader)
	pinned := map[string]ed25519.PublicKey{"dev-store": storePub}
	if storeKeys != "" {
		external, err := membership.ParseKeys(storeKeys)
		if err != nil {
			return err
		}
		for id, k := range external {
			pinned[id] = k
		}
	}

	verifier, err := authn.NewVerifier(authn.Config{IssuerBase: base, Audience: audience})
	if err != nil {
		return err
	}
	var checker authz.Checker
	var tuples entitlement.Store
	if url := os.Getenv("OPENFGA_API_URL"); url != "" {
		fga, err := authz.NewOpenFGA(authz.Options{BaseURL: url, APIToken: os.Getenv("OPENFGA_API_TOKEN"),
			StoreID: os.Getenv("OPENFGA_STORE_ID"), ModelID: os.Getenv("OPENFGA_MODEL_ID"), Logger: log})
		if err != nil {
			return err
		}
		checker, tuples = fga, fga
	} else {
		d := &decisions{log: log, tuples: map[string]authz.Tuple{}}
		checker, tuples = d, d
	}
	repo := gitops.NewGitOps(filepath.Join(work, "checkout"), remote, cluster, gitops.Person{})
	storeVerifier, err := entitlement.NewVerifier(pinned, cluster)
	if err != nil {
		return err
	}
	director, err := api.New(api.Config{
		Authn: verifier, Authz: checker, Repo: repo, Log: log, EnforceEntitlements: enforce, Cluster: cluster,
		Store: &api.StoreConfig{Verifier: storeVerifier, Applier: &entitlement.Applier{Repo: repo, Store: tuples}},
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", director)
	mux.Handle("/healthz", director)
	mux.HandleFunc("GET /realms/{realm}/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &signKey.PublicKey, KeyID: "dev", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /dev/people", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(people)
	})
	mux.HandleFunc("GET /dev/token", func(w http.ResponseWriter, r *http.Request) {
		who := r.URL.Query().Get("user")
		p, ok := people[who]
		if !ok {
			http.Error(w, "unknown user; see /dev/people", http.StatusNotFound)
			return
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signKey},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "dev"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		token, err := jwt.Signed(signer).Claims(jwt.Claims{
			Issuer: base + "/realms/" + p.Realm, Subject: who, Audience: jwt.Audience{audience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(8 * time.Hour)),
		}).Claims(map[string]any{"typ": "Bearer", "sid": "dev-" + who, "azp": "gentian-desktop",
			"name": p.Name, "email": who + "@example.com"}).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token, "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /dev/statement", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		granted := q.Get("granted") != "false"
		now := time.Now()
		c := entitlement.Claims{
			Issuer: "director-dev", Audience: "cluster:" + cluster, Subject: "tenant:" + q.Get("tenant"),
			ID: fmt.Sprintf("dev-%d", now.UnixNano()), IssuedAt: now.Unix(),
			Coordinate: q.Get("coordinate"), Granted: granted,
		}
		if granted {
			c.Expiry = now.Add(30 * 24 * time.Hour).Unix()
		} else {
			c.Reason = "revoked from director-dev"
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: storeKey},
			(&jose.SignerOptions{}).WithHeader("kid", "dev-store"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		payload, _ := json.Marshal(c)
		jws, err := signer.Sign(payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		compact, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"grant": compact})
	})

	fmt.Printf(`
director-dev — the real director API with local stand-ins

  API          %[1]s/v1/...
  people       curl %[1]s/dev/people
  a token      curl '%[1]s/dev/token?user=tom'
  a statement  curl '%[1]s/dev/statement?tenant=demo&coordinate=main/wiki'
  the repo     git --git-dir %[2]s log --format='%%h %%an | %%s%%n  %%(trailers:key=Gentian-Authz,valueonly)' main

  TOKEN=$(curl -s '%[1]s/dev/token?user=tom' | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
  curl -H "Authorization: Bearer $TOKEN" %[1]s/v1/tenants/demo/apps
  curl -H "Authorization: Bearer $TOKEN" -X POST %[1]s/v1/tenants/demo/apps/element

`, base, remote)

	srv := &http.Server{Addr: listen, Handler: cors(mux, origins), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// cors lets a UI dev server on another origin call the API. The production
// director sets no CORS headers: its callers are same-origin behind the gateway.
func cors(next http.Handler, origins []string) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); allowed[o] {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// seed creates the bare repository with tenants demo and solo.
func seed(work, cluster string) (string, error) {
	remote := filepath.Join(work, "gentian-deployments.git")
	src := filepath.Join(work, "seed")
	steps := [][]string{
		{"init", "-q", "--bare", "--initial-branch=main", remote},
		{"clone", "-q", remote, src},
	}
	for _, s := range steps {
		if out, err := exec.Command("git", s...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %w: %s", s, err, out)
		}
	}
	claims := filepath.Join(src, "clusters", cluster, "kernel", "claims")
	if err := os.MkdirAll(claims, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(claims, "cluster.yaml"), []byte("apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: "+cluster+"\nspec:\n  kernelDomain: k.example\n"), 0o644); err != nil {
		return "", err
	}
	for tenant, apps := range map[string][]string{"demo": {"nextcloud", "element"}, "solo": {"wiki"}} {
		dir := filepath.Join(src, "clusters", cluster, "tenants", tenant)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		y := "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: " + tenant +
			"\nspec:\n  displayName: " + tenant + "\n  apps:\n"
		for _, a := range apps {
			y += "  - profile: " + a + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "tenant.yaml"), []byte(y), 0o644); err != nil {
			return "", err
		}
	}
	for _, s := range [][]string{
		{"-C", src, "add", "-A"},
		{"-C", src, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-q", "-m", "seed"},
		{"-C", src, "push", "-q", "origin", "HEAD:main"},
	} {
		if out, err := exec.Command("git", s...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %w: %s", s, err, out)
		}
	}
	return remote, nil
}
