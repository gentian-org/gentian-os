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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
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
}

func start(t *testing.T, entitlements bool) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "tenant-demo", "tenant-solo")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	remote := dt.Remote(t, "demo", "solo", "other")
	repo := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, gitops.Person{})
	srv, err := api.New(api.Config{
		Authn: v, Authz: checker(t), Repo: repo, EnforceEntitlements: entitlements,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{Server: httptest.NewServer(srv), issuer: is, remote: remote}
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
	defer resp.Body.Close()
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
	resp.Body.Close()
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
	resp.Body.Close()
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
	resp.Body.Close()
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
	resp.Body.Close()
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
	"user:alice can_install_app tenant:demo":               true,
	"user:alice can_view tenant:demo":                      true,
	"user:tina can_install_app tenant:solo":                true,
	"user:tina can_view tenant:solo":                       true,
	"tenant:demo can_install catalogue_entry:main/element": true,
}

func checker(t *testing.T) authz.Checker {
	t.Helper()
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		return facts
	}
	storeID, modelID := loadOpenFGA(t, base)
	c, err := authz.NewOpenFGA(authz.Options{
		BaseURL: base, StoreID: storeID, ModelID: modelID,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
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
		defer resp.Body.Close()
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
	post("/stores/"+store.ID+"/write", map[string]any{
		"authorization_model_id": written.ID,
		"writes":                 map[string]any{"tuple_keys": tuples},
	}, nil)
	return store.ID, written.ID
}
