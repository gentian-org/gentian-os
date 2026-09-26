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

package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// The contract tests run the whole director — token verification, the OpenFGA
// question, the git write — against a static-key issuer and a bare remote.
//
// The people are those of authz/model/v1/tests.fga.yaml: tom administers
// tenant demo, mia is a member of it, tina administers tenant solo, alice is
// the platform administrator (who reaches demo through operated_by, and solo
// not at all), olaf belongs to another tenant.
//
// With DIRECTOR_TEST_OPENFGA_URL set, decisions come from a real OpenFGA
// loaded with model v1 and that fixture (make test-director-contract). Without
// it they come from a table that states the same facts, so the suite still
// runs where no container can.

const audience = "gentian-director"

type harness struct {
	*httptest.Server
	issuer   *dt.Issuer
	remote   string
	storeKey ed25519.PrivateKey
}

func start(t *testing.T, entitlements bool) *harness {
	t.Helper()
	return startWithTiles(t, entitlements, projectedTiles(t))
}

// projectedTiles writes what the operator would have projected on this
// cluster: the three kernel consoles it routes, and one tile belonging to an
// installed component, whose relation is held on the tenant rather than on the
// cluster. The director reads a file in a cluster too, so the tests read one.
func projectedTiles(t *testing.T) string {
	t.Helper()
	body, err := tilecatalogue.Marshal(tilecatalogue.Catalogue{Tiles: []tilecatalogue.Tile{
		{
			Name: "headlamp", DisplayName: "Cluster", Description: "The cluster as Kubernetes sees it.",
			Icon: "cluster", URL: "https://headlamp." + dt.KernelDomain + "/",
			Object: "cluster:" + dt.Cluster,
			AnyOf:  []string{"can_configure", "can_operate_system", "can_audit"},
		},
		{
			Name: "argocd", DisplayName: "Deployments", Description: "What git says the cluster should run.",
			Icon: "sync", URL: "https://argocd." + dt.KernelDomain + "/auth/login",
			Object: "cluster:" + dt.Cluster,
			AnyOf:  []string{"can_configure", "can_operate_system", "can_audit"},
		},
		{
			Name: "keycloak", DisplayName: "Identity", Description: "Realms, clients and the people in them.",
			Icon: "identity", URL: "https://id." + dt.KernelDomain + "/auth/admin/kernel/console/",
			Object: "cluster:" + dt.Cluster,
			AnyOf:  []string{"can_configure"},
		},
		{
			Name: "demo/notes/web", DisplayName: "Notes", Description: "A tenant's own app.",
			Icon: "notes", URL: "https://notes.demo." + dt.KernelDomain + "/",
			Object: "tenant:demo",
			AnyOf:  []string{"can_administer"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), tilecatalogue.Key)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startWithTiles(t *testing.T, entitlements bool, tilesPath string) *harness {
	t.Helper()
	return startWith(t, entitlements, tilesPath, nil)
}

// startWith is the harness with an operator to ask. lc is what answers the
// app-lifecycle API's reads; nil leaves the resources routes unregistered,
// which is what a director configured without one does.
func startWith(t *testing.T, entitlements bool, tilesPath string, lc api.Lifecycle) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "tenant-demo", "tenant-solo")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	remote := dt.Remote(t, "demo", "solo", "other")
	repo := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, gitops.Person{})
	decisions, tuples := checker(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	verifier, err := entitlement.NewVerifier(map[string]ed25519.PublicKey{"store-1": pub}, dt.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Config{
		Authn: v, Authz: decisions, Viewer: fixedViewer{}, Repo: repo, EnforceEntitlements: entitlements, Cluster: dt.Cluster,
		Store:     &api.StoreConfig{Verifier: verifier, Applier: &entitlement.Applier{Repo: repo, Store: tuples}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		TilesPath: tilesPath,
		Lifecycle: lc,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{Server: httptest.NewServer(srv), issuer: is, remote: remote, storeKey: priv}
	t.Cleanup(h.Close)
	return h
}

func (h *harness) token(t *testing.T, realm, sub string) string {
	return h.issuer.Token(t, dt.Claims{Realm: realm, Subject: sub, Audience: audience, Name: strings.ToUpper(sub[:1]) + sub[1:], Email: sub + "@example.com"})
}

func (h *harness) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, h.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (h *harness) tip(t *testing.T) string {
	return dt.Git(t, "", "--git-dir", h.remote, "rev-parse", "main")
}

func TestNoTokenNoWrite(t *testing.T) {
	h := start(t, false)
	before := h.tip(t)
	for name, tok := range map[string]string{
		"no token":       "",
		"garbage":        "abc",
		"wrong audience": h.issuer.Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: "nextcloud"}),
		"foreign issuer": dt.NewIssuer(t, "tenant-demo").Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: audience}),
		"forged key":     h.issuer.Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: audience, Key: dt.OtherKey(t)}),
	} {
		code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tok, "")
		if code != http.StatusUnauthorized {
			t.Errorf("%s: %d %v", name, code, body)
		}
		if code, _ := h.do(t, "GET", "/v1/tenants/demo/apps", tok, ""); code != http.StatusUnauthorized {
			t.Errorf("%s: read answered %d", name, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("an unauthenticated request moved the repository")
	}
}

// The header the old endpoint trusted is not an identity.
func TestAnActorHeaderIsNotAnIdentity(t *testing.T) {
	h := start(t, false)
	req, _ := http.NewRequest("POST", h.URL+"/v1/tenants/demo/apps/element", nil)
	req.Header.Set("X-Gentian-Actor", "tom")
	req.Header.Set("X-Forwarded-User", "tom")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestATenantAdminWritesTheirTenantAndNoOther(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	for _, path := range []string{"/v1/tenants/solo/apps/element", "/v1/tenants/other/apps/element"} {
		if code, body := h.do(t, "POST", path, tom, ""); code != http.StatusForbidden {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	if code, _ := h.do(t, "DELETE", "/v1/tenants/solo/apps/nextcloud", tom, ""); code != http.StatusForbidden {
		t.Fatalf("delete in another tenant: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/solo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`); code != http.StatusForbidden {
		t.Fatalf("addons in another tenant: %d", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/solo/apps", tom, ""); code != http.StatusForbidden {
		t.Fatalf("read of another tenant: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a forbidden request moved the repository")
	}

	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, "")
	if code != http.StatusAccepted || body["status"] != "installed" {
		t.Fatalf("own tenant: %d %v", code, body)
	}
	if body["commit"] != h.tip(t) {
		t.Fatalf("answer names %v, remote is at %s", body["commit"], h.tip(t))
	}
}

func TestTheCommitRecordsTheHumanTheDirectorAndTheDecision(t *testing.T) {
	h := start(t, false)
	req, _ := http.NewRequest("POST", h.URL+"/v1/tenants/demo/apps/element", nil)
	req.Header.Set("Authorization", "Bearer "+h.token(t, "tenant-demo", "tom"))
	req.Header.Set("X-Request-Id", "gw-0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("X-Request-Id") != "gw-0123456789abcdef" {
		t.Fatalf("status %d, request id %q", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	got := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an <%ae>|%cn|%(trailers:key=Gentian-Authz,valueonly)", "main")
	want := "Tom <tom@example.com>|gentian-director|req=gw-0123456789abcdef user:tom can_install_app tenant:demo allowed"
	if strings.TrimSpace(got) != want {
		t.Fatalf("commit =\n %s\nwant\n %s", got, want)
	}
}

// A request id arrives from outside and ends in a commit message.
func TestARequestIDThatIsNotAnIDIsReplaced(t *testing.T) {
	h := start(t, false)
	req, _ := http.NewRequest("POST", h.URL+"/v1/tenants/demo/apps/element", nil)
	req.Header.Set("Authorization", "Bearer "+h.token(t, "tenant-demo", "tom"))
	req.Header.Set("X-Request-Id", "x user:root can_configure cluster:main allowed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%(trailers:key=Gentian-Authz,valueonly)", "main")
	if strings.Contains(trailer, "root") || !strings.Contains(trailer, "user:tom can_install_app tenant:demo allowed") {
		t.Fatalf("trailer = %q", trailer)
	}
}

func TestAMemberReadsButDoesNotWrite(t *testing.T) {
	h := start(t, false)
	mia := h.token(t, "tenant-demo", "mia")
	code, body := h.do(t, "GET", "/v1/tenants/demo/apps", mia, "")
	if code != http.StatusOK || fmt.Sprint(body["apps"]) != "[map[profile:nextcloud]]" {
		t.Fatalf("read: %d %v", code, body)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/element", mia, ""); code != http.StatusForbidden {
		t.Fatalf("write: %d", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/apps", h.token(t, "tenant-demo", "olaf"), ""); code != http.StatusForbidden {
		t.Fatalf("a stranger read the tenant: %d", code)
	}
}

// The platform administrator reaches a tenant through operated_by and only
// through it: a tenant that withdrew the tuple is closed to them.
func TestThePlatformAdminReachesOnlyTenantsThatAreOperated(t *testing.T) {
	h := start(t, false)
	alice := h.token(t, "gentian", "alice")
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", alice, ""); code != http.StatusAccepted {
		t.Fatalf("operated tenant: %d %v", code, body)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/element", alice, ""); code != http.StatusForbidden {
		t.Fatalf("self-administered tenant: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/element", h.token(t, "tenant-solo", "tina"), ""); code != http.StatusAccepted {
		t.Fatalf("its own admin: %d", code)
	}
}

func TestInstallRequiresAnEntitlementThatHasNotExpired(t *testing.T) {
	h := start(t, true)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for name, body := range map[string]string{
		"no coordinate":  ``,
		"not entitled":   `{"coordinate":"main/odoo"}`,
		"expired":        `{"coordinate":"main/lapsed"}`,
		"bad coordinate": `{"coordinate":"main:element"}`,
	} {
		code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body)
		if code != http.StatusForbidden && code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("an unentitled install moved the repository")
	}
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"coordinate":"main/element"}`); code != http.StatusAccepted {
		t.Fatalf("entitled: %d %v", code, body)
	}
	// Another tenant's entitlement is not this tenant's.
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/element", h.token(t, "tenant-solo", "tina"), `{"coordinate":"main/element"}`); code != http.StatusForbidden {
		t.Fatalf("solo installed on demo's entitlement: %d", code)
	}
}

// ---- entitlements -----------------------------------------------------------

func (h *harness) statement(t *testing.T, c entitlement.Claims) string {
	t.Helper()
	if c.Audience == "" {
		c.Audience = "cluster:" + dt.Cluster
	}
	if c.Subject == "" {
		c.Subject = "tenant:demo"
	}
	if c.IssuedAt == 0 {
		c.IssuedAt = time.Now().Add(-time.Minute).Unix()
	}
	if c.Granted && c.Expiry == 0 {
		c.Expiry = time.Now().Add(24 * time.Hour).Unix()
	}
	raw, _ := json.Marshal(map[string]string{"grant": dt.Statement(t, h.storeKey, "store-1", c)})
	return string(raw)
}

// The store says yes, a person who may install delivers it, and only then does
// the install go through. Later the store says no, by itself, and it stops.
func TestAnEntitlementIsGrantedAndRevokedOnTheSamePath(t *testing.T) {
	h := start(t, true)
	tom, mia := h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-demo", "mia")
	install := func() int {
		code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"coordinate":"main/wiki"}`)
		return code
	}
	if code := install(); code != http.StatusForbidden {
		t.Fatalf("install before any grant: %d", code)
	}

	grant := h.statement(t, entitlement.Claims{ID: "grant-0001", Coordinate: "main/wiki", Granted: true})
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/entitlements", "", grant); code != http.StatusUnauthorized {
		t.Fatalf("a grant nobody delivered: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/entitlements", mia, grant); code != http.StatusForbidden {
		t.Fatalf("a grant delivered by a member: %d", code)
	}
	code, body := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, grant)
	if code != http.StatusAccepted || body["status"] != "recorded" {
		t.Fatalf("grant: %d %v", code, body)
	}
	if code, body := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, grant); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("the same grant again: %d %v", code, body)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "user:tom can_install_app tenant:demo; store:store-1 signed grant-0001 allowed") {
		t.Fatalf("trailer = %q", trailer)
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/entitlements", mia, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(body["entitlements"]), "coordinate:main/wiki") {
		t.Fatalf("read: %d %v", code, body)
	}
	if code := install(); code != http.StatusAccepted {
		t.Fatalf("install under the grant: %d", code)
	}

	// The revocation needs no one's consent.
	revoke := h.statement(t, entitlement.Claims{ID: "grant-0002", Coordinate: "main/wiki", Reason: "refund",
		IssuedAt: time.Now().Unix()})
	if code, body := h.do(t, "POST", "/v1/tenants/demo/entitlements", "", revoke); code != http.StatusAccepted {
		t.Fatalf("revocation: %d %v", code, body)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/wiki2", tom, `{"coordinate":"main/wiki"}`); code != http.StatusForbidden {
		t.Fatalf("install after the revocation: %d", code)
	}
	// Nor does delivering the old grant again bring it back.
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, grant); code != http.StatusConflict {
		t.Fatalf("the old grant replayed: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/wiki2", tom, `{"coordinate":"main/wiki"}`); code != http.StatusForbidden {
		t.Fatalf("install after the replay: %d", code)
	}
	if got := dt.RemoteFile(t, h.remote, "clusters/"+dt.Cluster+"/tenants/demo/entitlements.yaml"); !strings.Contains(got, "granted: false") || !strings.Contains(got, "reason: refund") {
		t.Fatalf("entitlements.yaml =\n%s", got)
	}
}

func TestOnlyTheStoresOwnStatementsAboutThisTenantAreBelieved(t *testing.T) {
	h := start(t, true)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	wrap := func(jws string) string {
		raw, _ := json.Marshal(map[string]string{"grant": jws})
		return string(raw)
	}
	ok := entitlement.Claims{Audience: "cluster:" + dt.Cluster, Subject: "tenant:demo", ID: "grant-0009", Coordinate: "main/wiki",
		Granted: true, IssuedAt: time.Now().Add(-time.Minute).Unix(), Expiry: time.Now().Add(time.Hour).Unix()}
	with := func(f func(*entitlement.Claims)) entitlement.Claims { c := ok; f(&c); return c }

	cases := map[string]struct {
		body string
		want int
	}{
		"signed by someone else":      {wrap(dt.Statement(t, stranger, "store-1", ok)), http.StatusUnauthorized},
		"a key id nobody pinned":      {wrap(dt.Statement(t, h.storeKey, "store-9", ok)), http.StatusUnauthorized},
		"not a statement":             {wrap("a.b.c"), http.StatusUnauthorized},
		"for another cluster":         {h.statement(t, with(func(c *entitlement.Claims) { c.Audience = "cluster:elsewhere" })), http.StatusForbidden},
		"for another tenant":          {h.statement(t, with(func(c *entitlement.Claims) { c.Subject = "tenant:solo" })), http.StatusForbidden},
		"a grant with no end":         {wrap(dt.Statement(t, h.storeKey, "store-1", with(func(c *entitlement.Claims) { c.Expiry = 0 }))), http.StatusBadRequest},
		"a revocation with no reason": {wrap(dt.Statement(t, h.storeKey, "store-1", with(func(c *entitlement.Claims) { c.Granted = false }))), http.StatusBadRequest},
		"dated tomorrow": {h.statement(t, with(func(c *entitlement.Claims) {
			c.IssuedAt = time.Now().Add(24 * time.Hour).Unix()
			c.Expiry = time.Now().Add(48 * time.Hour).Unix()
		})), http.StatusBadRequest},
		"an entry that is not one": {h.statement(t, with(func(c *entitlement.Claims) { c.Coordinate = "wiki#can_install" })), http.StatusBadRequest},
	}
	for name, c := range cases {
		if code, body := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", name, code, body, c.want)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused statement moved the repository")
	}
}

// A grant that has run out is recorded like any other and entitles to nothing.
func TestAnExpiredGrantEntitlesToNothing(t *testing.T) {
	h := start(t, true)
	tom := h.token(t, "tenant-demo", "tom")
	old := h.statement(t, entitlement.Claims{ID: "grant-0003", Coordinate: "main/old", Granted: true,
		IssuedAt: time.Now().Add(-48 * time.Hour).Unix(), Expiry: time.Now().Add(-24 * time.Hour).Unix()})
	if code, body := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, old); code != http.StatusAccepted {
		t.Fatalf("recording: %d %v", code, body)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/old", tom, `{"coordinate":"main/old"}`); code != http.StatusForbidden {
		t.Fatalf("install under an expired grant: %d", code)
	}
}

// The tiles a person sees follow from that person's relations, and from
// nothing the portal decides for itself.
//
// The catalogue itself is the operator's: this test hands the director the
// file the operator would have projected, which is how the director gets it in
// a cluster. What is under test here is the filtering, and that each tile is
// filtered against its own object, so a tenant's app tile is not answered by a
// relation on the cluster.
func TestTilesFollowTheCallersRelations(t *testing.T) {
	h := start(t, false)
	names := func(sub string) (int, []string) {
		code, body := h.do(t, "GET", "/v1/clusters/demo-cluster/tiles", h.token(t, "gentian", sub), "")
		if code != http.StatusOK {
			return code, nil
		}
		var out []string
		for _, tile := range body["tiles"].([]any) {
			out = append(out, tile.(map[string]any)["name"].(string))
		}
		return code, out
	}
	cases := map[string]struct {
		subject string
		code    int
		tiles   string
	}{
		"the platform administrator opens every console and the tenant's app": {
			subject: "alice", code: http.StatusOK, tiles: "[headlamp argocd keycloak demo/notes/web]",
		},
		"an auditor sees what it may read and not the identity console": {
			subject: "audrey", code: http.StatusOK, tiles: "[headlamp argocd]",
		},
		"a service administrator without can_audit does not reach the endpoint": {
			subject: "serge", code: http.StatusForbidden,
		},
		"a tenant member has no cluster relation at all": {
			subject: "mia", code: http.StatusForbidden,
		},
	}
	for name, c := range cases {
		code, got := names(c.subject)
		if code != c.code {
			t.Errorf("%s: %d, want %d", name, code, c.code)
			continue
		}
		if c.code == http.StatusOK && fmt.Sprint(got) != c.tiles {
			t.Errorf("%s: %v, want %s", name, got, c.tiles)
		}
	}
	_, body := h.do(t, "GET", "/v1/clusters/demo-cluster/tiles", h.token(t, "gentian", "alice"), "")
	if body["kernelDomain"] != dt.KernelDomain || !strings.Contains(fmt.Sprint(body["tiles"]), "https://headlamp.k.example/") {
		t.Fatalf("body = %v", body)
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/other/tiles", h.token(t, "gentian", "alice"), ""); code != http.StatusBadRequest {
		t.Fatalf("another cluster id: %d", code)
	}
}

// A cluster whose operator has not projected yet has no catalogue to read, and
// that is not a failure: the console shows a desktop with nothing on it, which
// is the truth, instead of an error it cannot act on.
func TestTilesWithoutAProjection(t *testing.T) {
	cases := map[string]string{
		"nothing configured":       "",
		"configured but not there": filepath.Join(t.TempDir(), "never-written.yaml"),
	}
	for name, path := range cases {
		h := startWithTiles(t, false, path)
		code, body := h.do(t, "GET", "/v1/clusters/demo-cluster/tiles", h.token(t, "gentian", "alice"), "")
		if code != http.StatusOK {
			t.Errorf("%s: %d, want 200", name, code)
			continue
		}
		if got := body["tiles"].([]any); len(got) != 0 {
			t.Errorf("%s: %v, want no tiles", name, got)
		}
	}
}

func TestAddonsRoundTrip(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar","deck"]}`); code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, body)
	}
	code, body := h.do(t, "GET", "/v1/tenants/demo/apps/nextcloud/addons", h.token(t, "tenant-demo", "mia"), "")
	if code != http.StatusOK || fmt.Sprint(body["addons"]) != "[calendar deck]" {
		t.Fatalf("get: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar","deck"]}`); code != http.StatusOK || body["status"] != "no_change" {
		t.Fatalf("repeat: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["x\n  evil: true"]}`); code != http.StatusBadRequest {
		t.Fatalf("injection: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"modules":[]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
}

func TestNamesAndUnknownTenants(t *testing.T) {
	h := start(t, false)
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "POST", "/v1/tenants/Demo/apps/element", alice, ""); code != http.StatusBadRequest {
		t.Fatalf("bad tenant name: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/..%2F..%2Fx", h.token(t, "tenant-demo", "tom"), ""); code != http.StatusBadRequest {
		t.Fatalf("bad profile name: %d", code)
	}
}

func TestConcurrentRequestsAllLand(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = h.do(t, "POST", fmt.Sprintf("/v1/tenants/demo/apps/app-%d", i), tom, "")
		}(i)
	}
	wg.Wait()
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	for i, code := range codes {
		if code != http.StatusAccepted || !strings.Contains(file, fmt.Sprintf("- profile: app-%d", i)) {
			t.Errorf("app-%d: status %d, in file: %v", i, code, strings.Contains(file, fmt.Sprintf("- profile: app-%d", i)))
		}
	}
}

// An OpenFGA that cannot be reached denies.
func TestAnUnreachableDecisionPointDenies(t *testing.T) {
	is := dt.NewIssuer(t, "tenant-demo")
	v, _ := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	remote := dt.Remote(t, "demo")
	srv, err := api.New(api.Config{
		Authn: v, Authz: failing{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Repo: gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, gitops.Person{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/tenants/demo/apps/element", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: audience}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(dt.RemoteFile(t, remote, dt.TenantPath("demo")), "element") {
		t.Fatal("the write went through without a decision")
	}
}

type failing struct{}

func (failing) Check(context.Context, string, string, string, string) (bool, error) {
	return false, errors.New("connection refused")
}

// ---- decisions ------------------------------------------------------------

type table map[string]bool

func (tb table) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	return tb[user+" "+relation+" "+object], nil
}

// facts is what model v1 answers for the fixture, for the questions these
// tests ask. The OpenFGA run is what shows the table is not wishful.
var facts = table{
	"user:tom can_install_app tenant:demo":                 true,
	"user:tom can_view tenant:demo":                        true,
	"user:mia can_view tenant:demo":                        true,
	"user:mia can_enter tenant:demo":                       true,
	"user:tom can_enter tenant:demo":                       true,
	"user:tom can_administer tenant:demo":                  true,
	"user:tom can_set_plan tenant:demo":                    true,
	"user:alice can_set_plan tenant:demo":                  true,
	"user:tom can_set_policy tenant:demo":                  true,
	"user:tom can_grant tenant:demo":                       true,
	"user:alice can_set_policy tenant:demo":                true,
	"user:tom can_manage_users tenant:demo":                true,
	"user:alice can_enter tenant:demo":                     true,
	"user:alice can_administer tenant:demo":                true,
	"user:alice can_manage_users tenant:demo":              true,
	"user:alice can_install_app tenant:demo":               true,
	"user:alice can_view tenant:demo":                      true,
	"user:tina can_install_app tenant:solo":                true,
	"user:tina can_view tenant:solo":                       true,
	"user:alice can_audit cluster:demo-cluster":            true,
	"user:alice can_configure cluster:demo-cluster":        true,
	"user:audrey can_audit cluster:demo-cluster":           true,
	"user:serge can_operate_system cluster:demo-cluster":   true,
	"tenant:demo can_install catalogue_entry:main/element": true,
}

// checker returns what decides and what holds tuples. With OpenFGA they are
// the same thing. Without it, the table answers for people and a small tuple
// store answers for entitlements, evaluating grant_valid the way the model does.
func checker(t *testing.T) (authz.Checker, entitlement.Store) {
	t.Helper()
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		mem := &tupleStore{tuples: map[string]authz.Tuple{}}
		return withEntitlements{table: facts, store: mem}, mem
	}
	storeID, modelID := loadOpenFGA(t, base)
	c, err := authz.NewOpenFGA(authz.Options{
		BaseURL: base, StoreID: storeID, ModelID: modelID,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, c
}

type tupleStore struct {
	mu     sync.Mutex
	tuples map[string]authz.Tuple
}

func tupleKey(t authz.Tuple) string { return t.User + " " + t.Relation + " " + t.Object }

func (m *tupleStore) Read(_ context.Context, f authz.Tuple) ([]authz.Tuple, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tuples[tupleKey(f)]; ok {
		return []authz.Tuple{t}, nil
	}
	return nil, nil
}

func (m *tupleStore) Write(_ context.Context, writes, deletes []authz.Tuple) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range deletes {
		delete(m.tuples, tupleKey(t))
	}
	for _, t := range writes {
		m.tuples[tupleKey(t)] = t
	}
	return nil
}

type withEntitlements struct {
	table
	store *tupleStore
}

func (w withEntitlements) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	if relation != "can_install" {
		return w.table.Check(ctx, id, user, relation, object)
	}
	if ok, _ := w.table.Check(ctx, id, user, relation, object); ok {
		return true, nil
	}
	got, _ := w.store.Read(ctx, authz.Tuple{User: user, Relation: "entitled", Object: object})
	if len(got) == 0 || got[0].Condition == nil {
		return false, nil
	}
	until, err := time.Parse(time.RFC3339, fmt.Sprint(got[0].Condition.Context["expires_at"]))
	return err == nil && time.Now().Before(until), nil
}

// loadOpenFGA creates a store holding model v1 and the shared fixture, plus
// the two entitlements the install test needs: one in force, one lapsed.
func loadOpenFGA(t *testing.T, base string) (storeID, modelID string) {
	t.Helper()
	post := func(path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("openfga %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("openfga %s: %d %s", path, resp.StatusCode, raw)
		}
		if out != nil {
			_ = json.Unmarshal(raw, out)
		}
	}
	var store struct {
		ID string `json:"id"`
	}
	post("/stores", map[string]string{"name": t.Name()}, &store)

	raw, err := os.ReadFile("../../../authz/model/v1/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	post("/stores/"+store.ID+"/authorization-models", model, &written)

	raw, err = os.ReadFile("../../../authz/model/v1/tests.fga.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tuples []map[string]any `json:"tuples"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	entitled := func(entry, until string) map[string]any {
		return map[string]any{"user": "tenant:demo", "relation": "entitled", "object": "catalogue_entry:" + entry,
			"condition": map[string]any{"name": "grant_valid", "context": map[string]any{"expires_at": until}}}
	}
	tuples := append(fixture.Tuples, entitled("main/element", "2999-01-01T00:00:00Z"), entitled("main/lapsed", "2020-01-01T00:00:00Z"))
	// The fixture's cluster is cluster:main; this director serves dt.Cluster.
	// Bind the same platform groups to it so cluster verbs can be checked.
	for _, role := range [][2]string{{"admin", "admin"}, {"auditor", "auditor"}, {"service-admin", "service_admin"}, {"security", "security_officer"}} {
		tuples = append(tuples, map[string]any{"user": "group:gentian/platform/" + role[0] + "#member", "relation": role[1], "object": "cluster:" + dt.Cluster})
	}
	post("/stores/"+store.ID+"/write", map[string]any{
		"authorization_model_id": written.ID,
		"writes":                 map[string]any{"tuple_keys": tuples},
	}, nil)
	return store.ID, written.ID
}

// The desktop renders from what the director says the caller holds, and
// decides nothing itself: a member sees no admin tile because can_administer
// is false, not because a flag in the desktop hid it.
func TestTheDesktopReadsTheCallersRelations(t *testing.T) {
	h := start(t, false)
	code, body := h.do(t, "GET", "/v1/tenants/demo/me", h.token(t, "tenant-demo", "mia"), "")
	if code != http.StatusOK {
		t.Fatalf("mia: %d %v", code, body)
	}
	rel, _ := body["relations"].(map[string]any)
	if rel["can_enter"] != true || rel["can_administer"] != false || rel["can_install_app"] != false {
		t.Fatalf("mia's relations = %v", rel)
	}
	if body["subject"] != "mia" {
		t.Fatalf("subject = %v", body["subject"])
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/me", h.token(t, "tenant-demo", "tom"), "")
	rel, _ = body["relations"].(map[string]any)
	if code != http.StatusOK || rel["can_administer"] != true || rel["can_install_app"] != true {
		t.Fatalf("tom: %d %v", code, rel)
	}
	// A stranger cannot even ask.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/me", h.token(t, "tenant-demo", "olaf"), ""); code != http.StatusForbidden {
		t.Fatalf("olaf: %d", code)
	}
}

// Who a caller is at cluster scope is answered for anyone who can be
// identified, with every verb false for a person who holds nothing here.
// That is the ordinary answer for almost everyone who signs in, and a console
// renders it as "no platform screens" rather than as a failure.
func TestClusterMeAnswersAnyIdentifiedCaller(t *testing.T) {
	h := start(t, false)
	path := "/v1/clusters/" + dt.Cluster + "/me"

	code, body := h.do(t, "GET", path, h.token(t, "gentian", "alice"), "")
	if code != http.StatusOK {
		t.Fatalf("alice: %d %v", code, body)
	}
	rel := body["relations"].(map[string]any)
	if rel["can_configure"] != true || rel["can_audit"] != true || rel["can_operate_system"] != false {
		t.Fatalf("alice holds %v", rel)
	}
	if body["subject"] != "alice" || body["cluster"] != dt.Cluster {
		t.Fatalf("body = %v", body)
	}

	code, body = h.do(t, "GET", path, h.token(t, "gentian", "serge"), "")
	rel = body["relations"].(map[string]any)
	if code != http.StatusOK || rel["can_operate_system"] != true || rel["can_configure"] != false {
		t.Fatalf("serge: %d %v", code, rel)
	}

	// Nothing at all, and still an answer.
	code, body = h.do(t, "GET", path, h.token(t, "gentian", "nobody"), "")
	if code != http.StatusOK {
		t.Fatalf("nobody: %d %v", code, body)
	}
	for verb, held := range body["relations"].(map[string]any) {
		if held != false {
			t.Fatalf("nobody holds %s", verb)
		}
	}

	if code, _ := h.do(t, "GET", path, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/elsewhere/me", h.token(t, "gentian", "alice"), ""); code != http.StatusNotFound {
		t.Fatalf("another cluster: %d, want 404", code)
	}
}

// The cluster's settings are read by whoever may audit the cluster and
// changed by whoever may configure it, which is what model v1 defines
// can_configure as: the Cluster claim, plans, ceilings and network.
func TestClusterSettingsAreReadWidelyAndWrittenNarrowly(t *testing.T) {
	h := start(t, false)
	alice := h.token(t, "gentian", "alice") // platform administrator

	code, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/settings", alice, "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, body)
	}
	settings, _ := body["settings"].([]any)
	if len(settings) == 0 {
		t.Fatal("the catalogue is empty; a console has nothing to render")
	}
	// A setting the claim does not carry is answered with the default the
	// schema will apply, so "unset" can be rendered as what it means rather
	// than as a blank the reader has to go and look up.
	for _, s := range settings {
		m := s.(map[string]any)
		if m["path"] != "certificates.acmeEnv" {
			continue
		}
		if m["default"] != "production" {
			t.Fatalf("certificates.acmeEnv default = %v, the Cluster XRD says production", m["default"])
		}
	}

	// A change lands as a commit against the claim.
	code, body = h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", alice,
		`{"settings":{"mail.serviceMode":"external"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("write: %d %v", code, body)
	}

	// And it is visible on the next read.
	_, body = h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/settings", alice, "")
	found := ""
	for _, s := range body["settings"].([]any) {
		m := s.(map[string]any)
		if m["path"] == "mail.serviceMode" {
			found, _ = m["value"].(string)
		}
	}
	if found != "external" {
		t.Fatalf("the setting did not land: %q", found)
	}

	// A setting outside the allowlist is refused rather than written.
	if code, _ := h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", alice,
		`{"settings":{"masterPasswordSecretRef.name":"mine"}}`); code != http.StatusBadRequest {
		t.Fatalf("a secret reference was accepted as a setting: %d", code)
	}
	// So is a value the setting does not take.
	if code, _ := h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", alice,
		`{"settings":{"mail.serviceMode":"carrier-pigeon"}}`); code != http.StatusBadRequest {
		t.Fatalf("an unknown value was accepted: %d", code)
	}

	// A tenant administrator holds nothing over the cluster.
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/settings", tina, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant admin read the cluster's settings: %d", code)
	}
	if code, _ := h.do(t, "PATCH", "/v1/clusters/"+dt.Cluster+"/settings", tina,
		`{"settings":{"mail.serviceMode":"system"}}`); code != http.StatusForbidden {
		t.Fatalf("a tenant admin changed the cluster's settings: %d", code)
	}
}

// fixedViewer stands in for OpenFGA's read side. What the API test is about is
// who may reach the view and that it has no write surface; what it answers is
// the authz package's own test.
type fixedViewer struct{}

func (fixedViewer) ViewOf(_ context.Context, object string) (authz.View, error) {
	return authz.View{
		Object: object,
		Bindings: []authz.Binding{
			{Relation: "admin", Groups: []string{"gentian:platform:admins"}, Grants: []string{"can_configure"}},
			{Relation: "break_glass", Groups: nil, Grants: []string{"can_edit_raw"}},
		},
		Unheld: 1,
	}, nil
}

// The authorization view is read-only and guarded by the relation that governs
// reading the object it describes. It exists instead of OpenFGA's playground,
// which is a development tool with a write surface.
func TestTheAuthorizationViewIsReadOnlyAndGuarded(t *testing.T) {
	h := start(t, false)
	tom := h.token(t, "tenant-demo", "tom")
	// tina belongs to another tenant: she holds can_view on solo and nothing
	// at all on demo, which is what makes her the right refusal to assert.
	tina := h.token(t, "tenant-solo", "tina")

	// A tenant admin may read their own tenant's bindings.
	code, body := h.do(t, http.MethodGet, "/v1/tenants/demo/authorization", tom, "")
	if code != http.StatusOK {
		t.Fatalf("tom: %d %v", code, body)
	}
	if body["object"] != "tenant:demo" {
		t.Fatalf("object = %v", body["object"])
	}
	// Another tenant's administrator may not: a tenant's bindings are the
	// tenant's, and who holds what is not public within a cluster.
	if code, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/authorization", tina, ""); code != http.StatusForbidden {
		t.Fatalf("tina reached another tenant's view: %d", code)
	}
	// And neither may an unauthenticated caller.
	if code, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/authorization", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous reached the view: %d", code)
	}
	// No write surface: every other method is refused by the mux, not by a
	// handler that might one day grow one.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if code, _ := h.do(t, method, "/v1/tenants/demo/authorization", tom, "{}"); code != http.StatusMethodNotAllowed {
			t.Fatalf("%s on the view answered %d, want 405", method, code)
		}
	}
}
