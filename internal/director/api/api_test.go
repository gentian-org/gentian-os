/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
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
	issuer *dt.Issuer
	remote string
	// asked is every question put to the decision point, in order, as
	// "<user> <relation> <object>".
	asked *asked
}

// asked records the questions a Checker is put, so a test can say what a
// route asks and, as much to the point, what it does not.
type asked struct {
	authz.Checker
	mu   sync.Mutex
	seen []string
}

func (a *asked) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	a.mu.Lock()
	a.seen = append(a.seen, user+" "+relation+" "+object)
	a.mu.Unlock()
	return a.Checker.Check(ctx, id, user, relation, object)
}

func (a *asked) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = nil
}

func (a *asked) questions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func start(t *testing.T) *harness {
	t.Helper()
	return startWith(t, nil)
}

// startWithCatalogue is the harness with catalogues of the whole cluster, for
// the installs that materialise a profile on reference (AD-3). They are
// written on the Cluster claim, which is where the director reads them from
// when it needs them. The fetcher is given the test server's own client, so
// it trusts that certificate and no other -- and connects to the test
// server's loopback address, which the director's own client refuses.
// declared is the sources in the claim's order; without it they are the
// map's, by name.
func startWithCatalogue(
	t *testing.T, src *httptest.Server, sources map[string]string, declared ...gitops.CatalogueSource,
) *harness {
	t.Helper()
	if declared == nil {
		names := make([]string, 0, len(sources))
		for name := range sources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			declared = append(declared, gitops.CatalogueSource{Name: name, URL: sources[name]})
		}
	}
	return startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: claimWith(declared...)})
	}, withFetcher(src))
}

// withFetcher gives the director a fetcher that reaches the test server and
// accepts any address for a new catalogue. Address checks have tests of their
// own (catalogue/address_test.go, and catalogues_test.go with the real one).
func withFetcher(src *httptest.Server) func(*api.Config) {
	return func(cfg *api.Config) {
		f := catalogue.NewFetcher()
		if src != nil {
			f.Client = src.Client()
		}
		f.Vet = func(context.Context, string) error { return nil }
		// As cmd/director wires it: the repository the director writes is
		// the one a catalogue kept in it is read from.
		if repo, ok := cfg.Repo.(catalogue.Repository); ok {
			f.Repo = repo
		}
		cfg.Catalogue = f
		cfg.StoreURL = "https://store.example.com"
	}
}

// claimWith is the fixture Cluster claim naming catalogues of the cluster.
func claimWith(sources ...gitops.CatalogueSource) string {
	claim := "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: " + dt.Cluster +
		"\nspec:\n  kernelDomain: " + dt.KernelDomain + "\n"
	if len(sources) == 0 {
		return claim
	}
	claim += "  catalogue:\n    storeUrl: https://store.example.com\n    sources:\n"
	for _, src := range sources {
		claim += "      - name: " + src.Name + "\n        url: " + src.URL + "\n"
	}
	return claim
}

// startWith is the harness with an operator to ask. lc is what answers the
// app-lifecycle API's reads; nil leaves the resources routes unregistered,
// which is what a director configured without one does.
func startWith(t *testing.T, lc api.Lifecycle, opts ...func(*api.Config)) *harness {
	t.Helper()
	return startSeeded(t, lc, nil, opts...)
}

// startSeeded is startWith for a repository that holds more than the fixture
// does when the director first reads it. seed is given the remote before the
// director clones it.
func startSeeded(t *testing.T, lc api.Lifecycle, seed func(remote string), opts ...func(*api.Config)) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "tenant-demo", "tenant-solo", "tenant-user")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	remote := dt.Remote(t, "demo", "solo", "other")
	if seed != nil {
		seed(remote)
	}
	repo := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, gitops.Person{})
	decisions := &asked{Checker: checker(t)}
	cfg := api.Config{
		Authn: v, Authz: decisions, Viewer: fixedViewer{}, Repo: repo, Cluster: dt.Cluster,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Lifecycle: lc,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	srv, err := api.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Registered first so that it runs last of the two: the watchers are
	// stopped once the last request is answered, and before the checkout
	// they write to is removed.
	t.Cleanup(srv.Close)
	h := &harness{Server: httptest.NewServer(srv), issuer: is, remote: remote, asked: decisions}
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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

// Installing asks one question, of the person: may they install apps in this
// tenant. Nothing is asked about the app, the catalogue it comes from or the
// tenant's right to it -- the platform does no licence gating. (An install
// for everyone asks a second, about granting: install_for_everyone_test.go.)
func TestInstallAsksOnlyWhetherThePersonMayInstallInTheTenant(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	h.asked.reset()
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/odoo", tom, "")
	if code != http.StatusAccepted {
		t.Fatalf("install by the tenant's administrator: %d %v", code, body)
	}
	if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tom can_install_app tenant:demo" {
		t.Fatalf("an install asked %q, want the one question can_install_app on the tenant", got)
	}

	// The same request from somebody who may not install is refused, and
	// naming a coordinate does not change that.
	before := h.tip(t)
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", mia, `{"coordinate":"main/wiki"}`); code != http.StatusForbidden {
		t.Fatalf("install by a member: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
}

// The store's old routes are gone, not merely switched off.
func TestThereIsNoEntitlementRoute(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/entitlements", tom, `{"grant":"a.b.c"}`); code != http.StatusNotFound {
		t.Fatalf("POST entitlements = %d, want 404", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/entitlements", tom, ""); code != http.StatusNotFound {
		t.Fatalf("GET entitlements = %d, want 404", code)
	}
}

// The digest an install asks for is recorded with it: as a field of the
// tenant's app entry, not as part of the app's name.
func TestAnInstallRecordsTheDigestItAskedFor(t *testing.T) {
	// A source whose one entry can be republished, which is what a new build
	// of it is.
	served := elementProfile
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profiles/element.yaml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(served))
	}))
	t.Cleanup(src.Close)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	digest := sha(elementProfile)

	// Stated in capitals and recorded in the one spelling the schema admits.
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, strings.ToUpper(digest)))
	if code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, body)
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(file, "  - profile: element\n    digest: "+digest+"\n") {
		t.Fatalf("the digest is not a field of the entry:\n%s", file)
	}
	var doc struct {
		Spec struct {
			Apps []struct {
				Profile string `json:"profile"`
				Digest  string `json:"digest"`
			} `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", err, file)
	}
	if len(doc.Spec.Apps) == 0 || doc.Spec.Apps[0].Profile != "element" || doc.Spec.Apps[0].Digest != digest {
		t.Fatalf("apps = %+v", doc.Spec.Apps)
	}
	if subject := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "-1", "main"); !strings.Contains(subject, "install element at "+digest[:19]) {
		t.Fatalf("the commit does not say which build: %q", subject)
	}

	// It is read back with the install.
	code, body = h.do(t, "GET", "/v1/tenants/demo/apps", tom, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(body["apps"]), "digest:"+digest) {
		t.Fatalf("apps = %d %v", code, body)
	}

	// The same build again changes nothing; another build moves the pin and
	// leaves one entry.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, digest)); code != http.StatusOK || body["status"] != "already_installed" {
		t.Fatalf("the same build again: %d %v", code, body)
	}
	served = strings.Replace(elementProfile, `version: "1.0.0"`, `version: "1.0.1"`, 1)
	next := sha(served)
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, next)); code != http.StatusAccepted || body["status"] != "updated" {
		t.Fatalf("another build: %d %v", code, body)
	}
	file = dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if strings.Count(file, "- profile: element") != 1 || !strings.Contains(file, "    digest: "+next+"\n") || strings.Contains(file, digest) {
		t.Fatalf("the pin did not move in place:\n%s", file)
	}

	// Something that is not a digest is refused before anything is written.
	before := h.tip(t)
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"digest":"latest"}`); code != http.StatusBadRequest {
		t.Fatalf("a digest that is not one: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
}

func TestAddonsRoundTrip(t *testing.T) {
	h := start(t)
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
	h := start(t)
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "POST", "/v1/tenants/Demo/apps/element", alice, ""); code != http.StatusBadRequest {
		t.Fatalf("bad tenant name: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/..%2F..%2Fx", h.token(t, "tenant-demo", "tom"), ""); code != http.StatusBadRequest {
		t.Fatalf("bad profile name: %d", code)
	}
}

func TestConcurrentRequestsAllLand(t *testing.T) {
	h := start(t)
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
	defer srv.Close()
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

// facts is what model v1 answers for the fixture, for the questions these
// tests ask. The OpenFGA run is what shows the table is not wishful.
var facts = dt.Table{
	"user:tom can_install_app tenant:demo":       true,
	"user:tom can_view tenant:demo":              true,
	"user:mia can_view tenant:demo":              true,
	"user:mia can_enter tenant:demo":             true,
	"user:tom can_enter tenant:demo":             true,
	"user:tom can_administer tenant:demo":        true,
	"user:tom can_set_plan tenant:demo":          true,
	"user:alice can_set_plan tenant:demo":        true,
	"user:tom can_set_policy tenant:demo":        true,
	"user:tom can_grant tenant:demo":             true,
	"user:alice can_set_policy tenant:demo":      true,
	"user:tom can_manage_users tenant:demo":      true,
	"user:alice can_enter tenant:demo":           true,
	"user:alice can_administer tenant:demo":      true,
	"user:alice can_manage_users tenant:demo":    true,
	"user:alice can_install_app tenant:demo":     true,
	"user:alice can_view tenant:demo":            true,
	"user:tina can_install_app tenant:solo":      true,
	"user:tina can_view tenant:solo":             true,
	"user:tom can_approve_privilege tenant:demo": true,
	// Tenant user carries the name of the one user tenant of a
	// single-tenancy cluster. uma administers it, ulf is a member, and the
	// platform operates it as it does demo.
	"user:uma can_install_app tenant:user": true,
	"user:uma can_view tenant:user":        true,
	"user:ulf can_view tenant:user":        true,
	// Its admins approve what it publishes, as demo's do: in the fixture
	// both tenants are ones whose administrators the cluster's
	// administrator let approve (spec.perimeter.adminsApprove).
	"user:uma can_expose tenant:user":        true,
	"user:alice can_install_app tenant:user": true,
	"user:alice can_view tenant:user":        true,
	// Declaring where software comes from is asked as can_write_credential,
	// the relation the custodian asked when it kept the repositories. Tenant
	// solo administers itself, so alice holds it on demo and not on solo.
	"user:tom can_write_credential tenant:demo":   true,
	"user:alice can_write_credential tenant:demo": true,
	"user:tina can_write_credential tenant:solo":  true,
	// can_expose is the perimeter approver's (AD-6). pat holds it and nothing
	// else -- the model's own fixture for the role existing separately from
	// running the tenant. tom holds it because demo's administrators were
	// let approve; alice because she administers the cluster that operates
	// demo and user. In solo, which administers itself and where nobody
	// switched anything on, paul of the perimeter group holds it and neither
	// tina, its administrator, nor alice does.
	"user:tom can_expose tenant:demo":   true,
	"user:pat can_expose tenant:demo":   true,
	"user:alice can_expose tenant:demo": true,
	"user:alice can_expose tenant:user": true,
	"user:paul can_expose tenant:solo":  true,
	// sam is the security officer: cluster#can_approve, and can_view on every
	// tenant because can_audit reaches it. Deliberately NOT a tenant
	// administrator, which is the case the kind-scoped check exists for.
	"user:sam can_approve cluster:demo-cluster": true,
	"user:sam can_view tenant:demo":             true,
	// alice configures the cluster and is not a security officer -- the
	// model's own fixture says can_approve: false for her.
	"user:alice can_audit cluster:demo-cluster":            true,
	"user:alice can_configure cluster:demo-cluster":        true,
	"user:alice can_write_credential cluster:demo-cluster": true,
	"user:audrey can_audit cluster:demo-cluster":           true,
	"user:serge can_operate_system cluster:demo-cluster":   true,
}

// checker returns what decides: a real OpenFGA holding model v1 when one is
// configured, and otherwise the table.
func checker(t *testing.T) authz.Checker {
	t.Helper()
	return dt.Checker(t, facts)
}

// The desktop renders from what the director says the caller holds, and
// decides nothing itself: a member sees no admin tile because can_administer
// is false, not because a flag in the desktop hid it.
func TestTheDesktopReadsTheCallersRelations(t *testing.T) {
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
	h := start(t)
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
