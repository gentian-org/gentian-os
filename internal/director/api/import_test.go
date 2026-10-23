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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// importingOperator is the operator as an import sees it: a bundle whose
// manifest names a tenant, a tenant that becomes Ready some time after it is
// declared, apps that come up, and a restore that finishes.
type importingOperator struct {
	mu        sync.Mutex
	declared  bool
	readyAt   time.Time
	restored  int
	uploads   int
	restoreAt time.Time
	// manifest is what inspect answers; empty is the bundle of tenant
	// "imported" with one app the cluster already has.
	manifest string
	// restores are the restores started, by name, and what each was asked.
	restores map[string]string
	// failRestore makes a started restore end Failed.
	failRestore bool
	// apps is what apps/status answers; empty is element, up.
	apps string
	// domain and domainRefused are the tenant's custom domain as the
	// operator has taken it, and why it did not.
	domain, domainRefused string
}

const importedManifest = `{"schemaVersion":1,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd","apps":[{"profile":"element"}]},"apps":[]}`

func (o *importingOperator) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case strings.HasSuffix(path, "/apps/status"):
		if o.apps != "" {
			return http.StatusOK, []byte(o.apps), nil
		}
		// A component the platform places itself is listed too, and is not
		// what an import waits for: it was not in the bundle.
		return http.StatusOK, []byte(`{"apps":[{"profile":"element","origin":"install","ready":true,"phase":"Ready"},`+
			`{"profile":"desktop","origin":"default","ready":false,"phase":"Installing"}]}`), nil
	case strings.Contains(path, "/restores/"):
		name := path[strings.LastIndex(path, "/")+1:]
		if _, started := o.restores[name]; !started {
			return http.StatusNotFound, []byte(`{"detail":"restore not found"}`), nil
		}
		phase := "Running"
		if time.Now().After(o.restoreAt) {
			phase = "Ready"
			if o.failRestore {
				phase = "Failed"
			}
		}
		return http.StatusOK, []byte(`{"name":"` + name + `","phase":"` + phase + `","message":"1 app(s) restored","passwordResetRequired":true,"outsideGuarantee":"written by platform version 0.4"}`), nil
	case strings.HasPrefix(path, "/v1/tenants/"):
		if !o.declared || time.Now().Before(o.readyAt) {
			return http.StatusOK, []byte(`{"name":"imported","exists":false}`), nil
		}
		state, _ := json.Marshal(map[string]any{"name": "imported", "exists": true, "deletionPolicy": "Retain", "phase": "Ready",
			"domain": o.domain, "domainRefused": o.domainRefused})
		return http.StatusOK, state, nil
	}
	return http.StatusNotFound, []byte(`{}`), nil
}

func (o *importingOperator) Do(_ context.Context, path, _ string, body any) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	raw, _ := json.Marshal(body)
	switch {
	case path == "/v1/bundles/inspect":
		if !strings.Contains(string(raw), `"passphrase":"open sesame"`) {
			return http.StatusBadRequest, []byte(`{"detail":"the manifest could not be opened with that key"}`), nil
		}
		manifest := o.manifest
		if manifest == "" {
			manifest = importedManifest
		}
		return http.StatusOK, []byte(`{"bundle":{"bucket":"gentian-imports","prefix":"x"},"manifest":` + manifest + `}`), nil
	case strings.HasSuffix(path, "/actions/restore"):
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.Name == "" {
			return http.StatusBadRequest, []byte(`{"detail":"an import names its restore"}`), nil
		}
		if o.restores == nil {
			o.restores = map[string]string{}
		}
		o.restores[req.Name] = path + " " + string(raw)
		o.restored++
		o.restoreAt = time.Now().Add(100 * time.Millisecond)
		return http.StatusAccepted, []byte(`{"name":"` + req.Name + `","tenant":"imported","phase":""}`), nil
	}
	return http.StatusNotFound, nil, nil
}

func (o *importingOperator) Upload(_ context.Context, _, _ string, body io.Reader) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.uploads++
	_, _ = io.Copy(io.Discard, body)
	return http.StatusCreated, []byte(`{"bundle":{"bucket":"gentian-imports","prefix":"x"}}`), nil
}

func (o *importingOperator) Stream(context.Context, string) (*http.Response, error) {
	return nil, io.EOF
}

func (o *importingOperator) Plans(context.Context, string, bool) ([]lifecycle.Plan, error) {
	return nil, nil
}

// An import is Create plus Restore: the tenant is declared from the bundle's
// manifest as one commit, the restore starts only once the operator reports
// the tenant and its apps up, and the status route tells the story.
func TestAnImportDeclaresWaitsAndRestores(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/bundles", alice, "not really a tar")
	if code != http.StatusAccepted || op.uploads != 1 {
		t.Fatalf("upload = %d %v (uploads %d)", code, body, op.uploads)
	}

	wrongKey := `{"bundle":{"bucket":"gentian-imports","prefix":"x"},"decryption":{"passphrase":"nope"}}`
	if code, _ := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, wrongKey); code != http.StatusBadRequest {
		t.Fatalf("wrong key = %d, want 400", code)
	}
	if _, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/imported/tenant.yaml"); err == nil {
		t.Fatal("a tenant was declared from a bundle nobody could open")
	}

	op.mu.Lock()
	op.declared, op.readyAt = true, time.Now().Add(150*time.Millisecond)
	op.mu.Unlock()
	good := `{"bundle":{"bucket":"gentian-imports","prefix":"x"},"decryption":{"passphrase":"open sesame"}}`
	code, body = h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, good)
	if code != http.StatusAccepted || body["phase"] != "declared" || body["tenant"] != "imported" {
		t.Fatalf("import = %d %v", code, body)
	}
	manifest, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/imported/tenant.yaml")
	if err != nil || !strings.Contains(manifest, "displayName: Imported Ltd") || !strings.Contains(manifest, "profile: element") {
		t.Fatalf("declared manifest: %v\n%s", err, manifest)
	}

	deadline := time.Now().Add(5 * time.Second)
	var phases []string
	for {
		_, st := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants/imported/import", alice, "")
		phase, _ := st["phase"].(string)
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
		if phase == "ready" {
			if st["passwordResetRequired"] != true || !strings.HasPrefix(st["restore"].(string), "import-") ||
				st["outsideGuarantee"] != "written by platform version 0.4" {
				t.Fatalf("final status = %v", st)
			}
			break
		}
		if phase == "failed" || time.Now().After(deadline) {
			t.Fatalf("import did not finish: %v (phases %v)", st, phases)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op.restored != 1 {
		t.Fatalf("restore started %d times", op.restored)
	}
	if code, _ := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, good); code != http.StatusConflict {
		t.Fatalf("importing again = %d, want 409 (the tenant exists)", code)
	}
}

// waitImport polls the status route until the import says one of the phases.
func waitImport(t *testing.T, h *harness, token, tenant string, phases ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		_, last = h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants/"+tenant+"/import", token, "")
		phase, _ := last["phase"].(string)
		for _, want := range phases {
			if phase == want {
				return last
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the import of %s did not reach %v: %v", tenant, phases, last)
	return nil
}

const goodImport = `{"bundle":{"bucket":"gentian-imports","prefix":"x"},"decryption":{"passphrase":"open sesame"}`

// An import makes the tenant under the name its bundle carries and renames
// nothing: a request that names another is refused and commits nothing. The
// tenant's realm and prefixes follow from that name by the platform's rule,
// whatever the bundle's manifest states -- here demo's own names, in a
// bundle of demo2 -- and the restore is asked for in demo2 and nowhere else.
func TestAnImportTakesTheBundlesNameAndItsOwnNamesFollowFromIt(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	ofDemo := `{"schemaVersion":2,"tenant":"demo","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Demo Ltd",` +
		`"isolation":{"mode":"namespace","keycloakRealm":"demo","databasePrefix":"demo_","s3Prefix":"demo-"},"apps":[{"profile":"element"}]},"apps":[]}`
	op := &importingOperator{declared: true, manifest: ofDemo}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	originalBefore, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/demo/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// Under its own name it exists: refused, nothing changed.
	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`); code != http.StatusConflict {
		t.Fatalf("importing over the original = %d %v", code, body)
	}
	// Under another: refused, and said why.
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`,"name":"demo2"}`)
	if said, _ := body["error"].(string); code != http.StatusUnprocessableEntity || !strings.Contains(said, "does not rename") {
		t.Fatalf("import of demo's bundle as demo2 = %d %v", code, body)
	}
	if _, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/demo2/tenant.yaml"); err == nil {
		t.Fatal("a renamed tenant was committed")
	}

	op.mu.Lock()
	op.manifest = strings.Replace(ofDemo, `"tenant":"demo"`, `"tenant":"demo2"`, 1)
	op.mu.Unlock()
	code, body = h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`,"name":"demo2"}`)
	if code != http.StatusAccepted || body["tenant"] != "demo2" {
		t.Fatalf("import of demo2's bundle = %d %v", code, body)
	}
	manifest, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/demo2/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"keycloakRealm: demo2\n", "databasePrefix: demo2_\n", "s3Prefix: demo2-\n", "displayName: Demo Ltd", "profile: element"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the imported manifest lacks %q:\n%s", want, manifest)
		}
	}
	for _, not := range []string{"keycloakRealm: demo\n", "databasePrefix: demo_\n", "s3Prefix: demo-\n"} {
		if strings.Contains(manifest, not) {
			t.Errorf("the imported tenant has another tenant's name %q:\n%s", strings.TrimSpace(not), manifest)
		}
	}

	st := waitImport(t, h, alice, "demo2", "ready", "failed")
	if st["phase"] != "ready" {
		t.Fatalf("the import ended %v", st)
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if len(op.restores) != 1 {
		t.Fatalf("restores started: %v", op.restores)
	}
	for name, asked := range op.restores {
		if !strings.HasPrefix(asked, "/v1/tenants/demo2/actions/restore ") {
			t.Errorf("restore %s was asked for at %s: not in the new tenant", name, asked)
		}
		// And said to be into a tenant made new: that is what brings the data
		// of uninstalled apps with stores made for it, and what keeps a right
		// granted in the bundle's tenant from being granted here.
		if !strings.Contains(asked, `"intoNewTenant":true`) {
			t.Errorf("restore %s was not asked for as one into a tenant made new: %s", name, asked)
		}
	}
	originalAfter, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/demo/tenant.yaml")
	if err != nil || originalAfter != originalBefore {
		t.Errorf("the original tenant's manifest changed: %v\n%s", err, originalAfter)
	}
	// Finished: its record is gone from git.
	if _, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/demo2/"+gitops.ImportFile); err == nil {
		t.Error("the record of a finished import is still in git")
	}
}

// An import commits the definition of an app before the tenant that names
// it, as an install does: from a catalogue this cluster declares, at the
// build the bundle records -- whichever of the cluster's catalogues serves
// those bytes, since the catalogue's name on the cluster the bundle came
// from says nothing here.
func TestAnImportMaterialisesTheProfilesTheBundleRecords(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	src := catalogueSource(t, elementProfile)
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":2,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd",` +
			`"apps":[{"profile":"element","digest":"` + sha(elementProfile) + `","catalogue":"theirs"}]},"apps":[]}`}
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "main", URL: src.URL})})
	}, withFetcher(src))
	alice := h.token(t, "gentian", "alice")

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	got := dt.RemoteFile(t, h.remote, dt.CataloguePath("element.yaml"))
	if sha(strings.TrimRight(got, "\n")+"\n") != sha(elementProfile) {
		t.Fatalf("the profile committed for the import is not the build the bundle records:\n%s", got)
	}
	bundleFile := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("element")))
	if !strings.Contains(bundleFile, "cluster/main") {
		t.Errorf("the profile is not recorded as fetched from this cluster's catalogue:\n%s", bundleFile)
	}
	// The profile's commit comes before the tenant's.
	log := dt.Git(t, "", "--git-dir", h.remote, "log", "--format=%s", "main")
	tenantAt, profileAt := strings.Index(log, "Import tenant imported"), strings.Index(log, "element")
	if tenantAt < 0 || profileAt < 0 || profileAt < tenantAt {
		t.Errorf("the tenant was committed before its app's definition (newest first):\n%s", log)
	}
	waitImport(t, h, alice, "imported", "ready")
}

// What the cluster cannot obtain is said before anything is changed: every
// profile, by name, with why. It used to be found out two hours later, by a
// tenant that never left Degraded.
func TestAnImportIsRefusedUpFrontWhenAProfileCannotBeObtained(t *testing.T) {
	src := catalogueSource(t, elementProfile)
	other := "sha256:" + strings.Repeat("ab", 32)
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":2,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd","apps":[` +
			`{"profile":"element","digest":"` + other + `","catalogue":"main"},` +
			`{"profile":"crm"},` +
			`{"profile":"nextcloud","addons":["nextcloud-talk"]}]},"apps":[]}`}
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "main", URL: src.URL})})
	}, withFetcher(src))
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d %v", code, body)
	}
	said, _ := body["detail"].(string)
	if said == "" {
		said, _ = body["error"].(string)
	}
	if said == "" {
		raw, _ := json.Marshal(body)
		said = string(raw)
	}
	for _, want := range []string{
		"app element at sha256:abababababab", "none of this cluster's catalogues serves that build: main",
		"app crm (the bundle records no build of it", "add-on nextcloud-talk of nextcloud", "Nothing was changed",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "app nextcloud (") {
		t.Errorf("a profile the cluster has is named as missing:\n%s", said)
	}
	if h.tip(t) != before {
		t.Error("a refused import still committed something")
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants/imported/import", alice, ""); code != http.StatusNotFound {
		t.Errorf("a refused import has a status: %d", code)
	}
}

// A tenant of an earlier release lists the platform's own per-tenant App
// Store among its apps, a profile this release no longer has. The import
// leaves it out and says so -- in the answer and on the status route, to the
// end -- and gives the tenant every other app.
func TestAnImportSkipsARetiredPlatformAppAndSaysSo(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":1,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd",` +
			`"apps":[{"profile":"app-store-me"},{"profile":"element"}]},"apps":[{"name":"app-store-me","profile":"app-store-me"},{"name":"element","profile":"element"}]}`}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	saysSkipped := func(where string, st map[string]any) {
		t.Helper()
		skipped, _ := st["skipped"].([]any)
		if len(skipped) != 1 {
			t.Fatalf("%s: skipped = %v, want app-store-me alone", where, st["skipped"])
		}
		entry, _ := skipped[0].(map[string]any)
		if entry["app"] != "app-store-me" || !strings.Contains(entry["reason"].(string), "no longer has") {
			t.Errorf("%s: skipped entry = %v", where, entry)
		}
	}
	saysSkipped("the answer", body)

	manifest, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/imported/tenant.yaml")
	if err != nil || !strings.Contains(manifest, "profile: element") || strings.Contains(manifest, "app-store-me") {
		t.Fatalf("declared manifest: %v\n%s", err, manifest)
	}
	record, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/imported/"+gitops.ImportFile)
	if err != nil || !strings.Contains(record, `"app": "app-store-me"`) {
		t.Errorf("the record of the import does not keep what was skipped: %v\n%s", err, record)
	}

	st := waitImport(t, h, alice, "imported", "ready", "failed")
	if st["phase"] != "ready" {
		t.Fatalf("the import ended %v", st)
	}
	saysSkipped("the finished status", st)
}

// Only the platform's own retired components are left out. An app of the
// tenant's whose definition cannot be had refuses the import as before, and
// the retired one is not named among what is missing.
func TestAnImportBesideARetiredPlatformAppStillRefusesAnUnknownApp(t *testing.T) {
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":1,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd",` +
			`"apps":[{"profile":"app-store-me"},{"profile":"crm"},{"profile":"element"}]},"apps":[]}`}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "app crm (") || strings.Contains(string(raw), "app-store-me") {
		t.Errorf("the refusal = %s", raw)
	}
	if h.tip(t) != before {
		t.Error("a refused import still committed something")
	}
}

// secondDirector is another start of the director on the same repository:
// what a restart is.
func secondDirector(t *testing.T, h *harness, lc api.Lifecycle) (*api.Server, *harness) {
	t.Helper()
	v, err := authn.NewVerifier(authn.Config{IssuerBase: h.issuer.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	repo := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	decisions := &asked{Checker: checker(t)}
	srv, err := api.New(api.Config{
		Authn: v, Authz: decisions, Viewer: fixedViewer{}, Repo: repo, Cluster: dt.Cluster,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Lifecycle: lc,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	next := &harness{Server: httptest.NewServer(srv), issuer: h.issuer, remote: h.remote, asked: decisions}
	t.Cleanup(next.Close)
	return srv, next
}

// The director restarts while an import waits for its tenant. The import is
// on record in git, so the next start knows of it. It has no key -- the key
// came with the request and was never written down -- and says so; asked
// again with the key, it goes on, and starts the one restore the import has.
func TestAnImportSurvivesARestartOfTheDirector(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	// The tenant is never provisioned for the first director.
	never := &importingOperator{}
	h := startWith(t, never)
	alice := h.token(t, "gentian", "alice")
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	restore, _ := body["restore"].(string)
	if !strings.HasPrefix(restore, "import-") {
		t.Fatalf("the import does not name its restore: %v", body)
	}
	waitImport(t, h, alice, "imported", "provisioning")

	// The restart: a director that knows nothing but what git says.
	op := &importingOperator{declared: true}
	srv, next := secondDirector(t, h, op)
	srv.ResumeImports(context.Background())
	st := waitImport(t, next, alice, "imported", "awaiting-key", "restoring", "ready", "failed")
	if st["phase"] != "awaiting-key" || !strings.Contains(st["message"].(string), "Ask for the import again") || st["restore"] != restore {
		t.Fatalf("after the restart the import says %v", st)
	}
	if op.restored != 0 {
		t.Fatal("a restore was started without the bundle's key")
	}

	// Another bundle is not this import.
	wrong := `{"bundle":{"bucket":"gentian-imports","prefix":"y"},"decryption":{"passphrase":"open sesame"}}`
	if code, body := next.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, wrong); code != http.StatusConflict {
		t.Fatalf("going on with another bundle = %d %v", code, body)
	}
	// Asked again with the key: it goes on, to the end.
	if code, body := next.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`); code != http.StatusAccepted {
		t.Fatalf("asking again = %d %v", code, body)
	}
	st = waitImport(t, next, alice, "imported", "ready", "failed")
	if st["phase"] != "ready" {
		t.Fatalf("the resumed import ended %v", st)
	}
	op.mu.Lock()
	if _, ok := op.restores[restore]; !ok || len(op.restores) != 1 {
		t.Errorf("the restore the import named was not the one started: %v", op.restores)
	}
	op.mu.Unlock()
	if _, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/tenants/imported/"+gitops.ImportFile); err == nil {
		t.Error("the record of a finished import is still in git")
	}
	// The first director is still running in this process, which it is not
	// after a real restart, and still waits for its tenant: let it see one, so
	// that its watcher ends before the test does.
	never.mu.Lock()
	never.declared = true
	never.mu.Unlock()
	waitImport(t, h, alice, "imported", "ready", "failed")
}

// The director restarts while the restore runs. Nothing is needed from
// anybody: the restore's name is on record, it is found running, and it is
// watched to its end. A second one is not started.
func TestAnImportWhoseRestoreWasStartedIsWatchedAfterARestart(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`); code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	waitImport(t, h, alice, "imported", "restoring")
	// The restore goes on for longer than the first director lives.
	op.mu.Lock()
	op.restoreAt = time.Now().Add(300 * time.Millisecond)
	op.mu.Unlock()

	srv, next := secondDirector(t, h, op)
	srv.ResumeImports(context.Background())
	st := waitImport(t, next, alice, "imported", "ready", "failed", "awaiting-key")
	if st["phase"] != "ready" {
		t.Fatalf("after the restart the import ended %v", st)
	}
	if op.restored != 1 {
		t.Errorf("%d restores were started for one import", op.restored)
	}
	// The first director is still running in this process, which it is not
	// after a real restart: let its watcher end before its checkout goes.
	waitImport(t, h, alice, "imported", "ready", "failed")
}

// An imported app that waits for a credential a person supplies -- which no
// bundle carries -- is named with the credential while the import waits, so
// that nobody waits the two hours out to learn why.
func TestAnImportWaitingForASuppliedCredentialNamesIt(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true,
		apps: `{"apps":[{"profile":"element","ready":false,"phase":"Installing","awaitingCredentials":[{"name":"payments","displayName":"Payment keys","requirement":"app-imported-element-payments"}]}]}`}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`); code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, st := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants/imported/import", alice, "")
		msg, _ := st["message"].(string)
		if strings.Contains(msg, "element (waits for the credentials a person supplies, which a bundle does not carry: Payment keys (payments)") &&
			strings.Contains(msg, "under Credentials") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import does not name the credential it waits for: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.restored != 0 {
		t.Error("the restore began before the app was up")
	}
}

// customDomainManifest is the bundle of a tenant bound to a custom domain.
var customDomainManifest = strings.Replace(importedManifest, `"createdAt"`, `"customDomain":"Imported.Example","createdAt"`, 1)

// An import keeps the custom domain the bundle's tenant had, unless asked
// not to: its TenantDomain is in the commit that declares the tenant, and the
// restore starts only once the operator has put the tenant on it -- Ready
// alone is not that -- so that the restore sees the domain the bundle was
// taken on. The status says the DNS is the person's to point.
func TestAnImportKeepsTheBundlesCustomDomain(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true, manifest: customDomainManifest}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusAccepted || body["customDomain"] != "imported.example" ||
		!strings.Contains(fmt.Sprint(body["message"]), "point the DNS of imported.example") {
		t.Fatalf("import = %d %v", code, body)
	}
	if doc := dt.RemoteFile(t, h.remote, domainFile("imported")); !strings.Contains(doc, "kind: TenantDomain") ||
		!strings.Contains(doc, "  domain: imported.example") {
		t.Fatalf("the custom domain is not declared:\n%s", doc)
	}
	if k := dt.RemoteFile(t, h.remote, strings.TrimSuffix(domainFile("imported"), gitops.TenantDomainFile)+"kustomization.yaml"); !strings.Contains(k, "- "+gitops.TenantDomainFile) {
		t.Fatalf("the kustomization does not list the TenantDomain:\n%s", k)
	}
	// One commit with the tenant: the tenant is never applied without it.
	files, err := exec.Command("git", "--git-dir", h.remote, "diff-tree", "--no-commit-id", "--name-only", "-r", "main").Output()
	if err != nil || !strings.Contains(string(files), "imported/tenant.yaml") || !strings.Contains(string(files), "imported/"+gitops.TenantDomainFile) {
		t.Fatalf("the tenant and its domain are not one commit: %v\n%s", err, files)
	}
	if h.tip(t) == before {
		t.Fatal("nothing was committed")
	}

	// Ready, apps up, and not yet on its domain: no restore.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, st := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants/imported/import", alice, "")
		if strings.Contains(fmt.Sprint(st["message"]), "put the tenant on its custom domain imported.example") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import does not wait for the domain: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	op.mu.Lock()
	if op.restored != 0 {
		op.mu.Unlock()
		t.Fatal("the restore began before the tenant was on its custom domain")
	}
	op.domain = "imported.example"
	op.mu.Unlock()
	st := waitImport(t, h, alice, "imported", "ready", "failed")
	if st["phase"] != "ready" || st["customDomain"] != "imported.example" ||
		!strings.Contains(fmt.Sprint(st["notes"]), "point the DNS of imported.example") {
		t.Fatalf("the import ended %v", st)
	}
}

// Asked not to keep it, an import makes the tenant at its default address,
// as before.
func TestAnImportAskedNotToKeepTheCustomDomainDoesNot(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true, manifest: customDomainManifest}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`,"keepCustomDomain":false}`)
	if code != http.StatusAccepted || body["customDomain"] != nil {
		t.Fatalf("import = %d %v", code, body)
	}
	if _, err := gitShow(h.remote, domainFile("imported")); err == nil {
		t.Fatal("a custom domain was declared that the import was asked not to keep")
	}
	if st := waitImport(t, h, alice, "imported", "ready", "failed"); st["phase"] != "ready" {
		t.Fatalf("the import ended %v", st)
	}
}

// A custom domain another tenant here is bound to refuses the import, with
// the way out, before anything is written -- the definitions an import
// commits first included.
func TestAnImportWhoseCustomDomainIsAnotherTenantsIsRefused(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	src := catalogueSource(t, elementProfile)
	op := &importingOperator{declared: true, manifest: strings.Replace(customDomainManifest,
		`"apps":[{"profile":"element"}]`, `"apps":[{"profile":"element","digest":"`+sha(elementProfile)+`","catalogue":"main"}]`, 1)}
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "main", URL: src.URL})})
	}, withFetcher(src))
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", domainRoute("demo"), alice, `{"domain":"imported.example"}`); code >= 300 {
		t.Fatalf("bind demo: %d %v", code, out)
	}
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	msg := fmt.Sprint(body["error"])
	if code != http.StatusConflict {
		t.Fatalf("import = %d %v, want 409", code, body)
	}
	for _, want := range []string{"imported.example is bound to tenant demo", "Nothing was changed", "--drop-custom-domain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q: %s", want, msg)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused import moved the repository")
	}
}

// A binding the operator refuses -- here, a TenantDomain applied by other
// means that claimed the domain first -- fails the import before a restore,
// which would otherwise see a move.
func TestAnImportWhoseCustomDomainTheOperatorRefusesFails(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true, manifest: customDomainManifest,
		domainRefused: "imported.example is tenant other's; the tenant stays at its default domain"}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`); code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	st := waitImport(t, h, alice, "imported", "ready", "failed")
	if st["phase"] != "failed" || !strings.Contains(fmt.Sprint(st["message"]), "is tenant other's") {
		t.Fatalf("the import ended %v", st)
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.restored != 0 {
		t.Error("a restore was started onto another domain")
	}
}

// A custom domain that cannot be bound here -- on this cluster's own domain --
// refuses the import with 422, and nothing is written.
func TestAnImportWhoseCustomDomainCannotBeBoundHereIsRefused(t *testing.T) {
	op := &importingOperator{declared: true, manifest: strings.Replace(importedManifest, `"createdAt"`,
		`"customDomain":"imported.`+dt.KernelDomain+`","createdAt"`, 1)}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if msg := fmt.Sprint(body["error"]); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "on the kernel domain") ||
		!strings.Contains(msg, "keepCustomDomain: false") {
		t.Fatalf("import = %d %v, want 422", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("a refused import moved the repository")
	}
}
