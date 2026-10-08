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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
}

const importedManifest = `{"schemaVersion":1,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd","apps":[{"profile":"element"}]},"apps":[]}`

func (o *importingOperator) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case strings.HasSuffix(path, "/apps/status"):
		return http.StatusOK, []byte(`{"apps":[{"profile":"element","ready":true,"phase":"Ready"}]}`), nil
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
		return http.StatusOK, []byte(`{"name":"` + name + `","phase":"` + phase + `","message":"1 app(s) restored","passwordResetRequired":true}`), nil
	case strings.HasPrefix(path, "/v1/tenants/"):
		if !o.declared || time.Now().Before(o.readyAt) {
			return http.StatusOK, []byte(`{"name":"imported","exists":false}`), nil
		}
		return http.StatusOK, []byte(`{"name":"imported","exists":true,"deletionPolicy":"Retain","phase":"Ready"}`), nil
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
			if st["passwordResetRequired"] != true || !strings.HasPrefix(st["restore"].(string), "import-") {
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

// A bundle of tenant "demo", imported as "demo2" on the cluster demo is on.
// The bundle's manifest states demo's own names, as an export of a created
// tenant does. The new tenant has its own; demo's manifest is untouched; and
// the restore is asked for in demo2 and nowhere else.
func TestAnImportUnderAnotherNameBesideTheOriginalLeavesTheOriginalAlone(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":2,"tenant":"demo","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Demo Ltd",` +
			`"isolation":{"mode":"namespace","keycloakRealm":"demo","databasePrefix":"demo_","s3Prefix":"demo-"},"apps":[{"profile":"element"}]},"apps":[]}`}
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

	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`,"name":"demo2"}`)
	if code != http.StatusAccepted || body["tenant"] != "demo2" {
		t.Fatalf("import as demo2 = %d %v", code, body)
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
			t.Errorf("the imported tenant has the original's name %q:\n%s", strings.TrimSpace(not), manifest)
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
