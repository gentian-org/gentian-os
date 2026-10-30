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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// sourcingOperator is the operator as the source routes see it: one source
// it holds, and what each call was asked.
type sourcingOperator struct {
	importingOperator
	smu   sync.Mutex
	calls []string
}

func (o *sourcingOperator) record(s string) {
	o.smu.Lock()
	defer o.smu.Unlock()
	o.calls = append(o.calls, s)
}

func (o *sourcingOperator) seen() string {
	o.smu.Lock()
	defer o.smu.Unlock()
	return strings.Join(o.calls, "\n")
}

func (o *sourcingOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	switch {
	case path == "/v1/bundle-sources":
		return http.StatusOK, []byte(`{"sources":[{"name":"left-behind","endpoint":"https://s3.example","bucket":"b","ready":true}]}`), nil
	case strings.Contains(path, "bundle-sources") || strings.HasPrefix(path, "/v1/bundle-fetches/"):
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		o.record("GET " + path)
		return http.StatusOK, []byte(`{"bundles":[]}`), nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

func (o *sourcingOperator) Do(ctx context.Context, path, actor string, body any) (int, []byte, error) {
	raw, _ := json.Marshal(body)
	o.record(path + " " + string(raw))
	switch {
	case path == "/v1/bundle-fetches":
		return http.StatusAccepted, []byte(`{"name":"bundle-fetch-1","phase":"running"}`), nil
	case strings.HasSuffix(path, "/actions/retire"):
		return http.StatusOK, []byte(`{"retired":true}`), nil
	case path == "/v1/bundles/inspect" && strings.Contains(string(raw), `"source"`):
		if !strings.Contains(string(raw), `"passphrase":"open sesame"`) {
			return http.StatusBadRequest, []byte(`{"detail":"the manifest could not be opened with that key"}`), nil
		}
		return http.StatusOK, []byte(`{"bundle":{"bucket":"old","prefix":"nightly","endpoint":"https://s3.example","credentialSecret":"bundle-source-old"},"manifest":` + importedManifest + `}`), nil
	}
	return o.importingOperator.Do(ctx, path, actor, body)
}

func (o *sourcingOperator) Stream(_ context.Context, path string) (*http.Response, error) {
	o.record("STREAM " + path)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("tar"))}, nil
}

func (o *sourcingOperator) Upload(_ context.Context, path, _ string, body io.Reader) (int, []byte, error) {
	_, _ = io.Copy(io.Discard, body)
	o.record("UPLOAD " + path)
	return http.StatusCreated, []byte(`{"bundle":{"bucket":"gentian-imports","prefix":"20261016-000000-aaaaaaaa"}}`), nil
}

// A source's address is a commit; the list says what git declares beside what
// the cluster holds; removing one commits and has the cluster let it go.
func TestTheClustersSourcesAreDeclaredInGitAndListedWithWhatTheClusterHolds(t *testing.T) {
	op := &sourcingOperator{}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	base := "/v1/clusters/" + dt.Cluster + "/bundle-sources"

	code, body := h.do(t, "PUT", base+"/old", alice, `{"endpoint":"https://s3.example","bucket":"old","accessKey":"AKIA"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a declaration carrying a key = %d %v", code, body)
	}
	code, body = h.do(t, "PUT", base+"/old", alice, `{"endpoint":"https://s3.example","bucket":"old"}`)
	if code != http.StatusAccepted || body["credentialName"] != "bundle-source-old" {
		t.Fatalf("declare = %d %v", code, body)
	}
	file, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/kernel/claims/bundle-source-old.yaml")
	if err != nil || !strings.Contains(file, "kind: BundleSource") {
		t.Fatalf("committed: %v\n%s", err, file)
	}

	code, body = h.do(t, "GET", base, alice, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, body)
	}
	sources, _ := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", body)
	}
	old, _ := sources[0].(map[string]any)
	left, _ := sources[1].(map[string]any)
	if old["name"] != "old" || old["declared"] != true || old["applied"] != false || old["bucket"] != "old" {
		t.Errorf("the declared source = %v", old)
	}
	if left["name"] != "left-behind" || left["declared"] != false || left["applied"] != true {
		t.Errorf("the source git no longer declares = %v", left)
	}

	code, body = h.do(t, "DELETE", base+"/old", alice, "")
	if code != http.StatusAccepted || body["deleted"] != true {
		t.Fatalf("remove = %d %v", code, body)
	}
	if _, err := gitShow(h.remote, "clusters/"+dt.Cluster+"/kernel/claims/bundle-source-old.yaml"); err == nil {
		t.Error("the declaration is still in git")
	}
	if !strings.Contains(op.seen(), "/v1/bundle-sources/old/actions/retire") {
		t.Errorf("the cluster was not asked to let it go: %s", op.seen())
	}
}

// A fetch names the source the route names, never one the body names, and a
// tenant's administrator reaches the tenant's own sources and not the
// cluster's.
func TestSourcesAreReachedUnderTheRelationOfTheirLevel(t *testing.T) {
	op := &sourcingOperator{}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")

	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/bundle-sources/old/actions/fetch", alice,
		`{"prefix":"nightly","source":{"name":"elsewhere"}}`); code != http.StatusBadRequest {
		t.Fatalf("a fetch whose body names a source = %d %v", code, body)
	}
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/bundle-sources/old/actions/fetch", alice, `{"prefix":"nightly"}`)
	if code != http.StatusAccepted || body["name"] != "bundle-fetch-1" {
		t.Fatalf("fetch = %d %v", code, body)
	}
	if !strings.Contains(op.seen(), `/v1/bundle-fetches {"source":{"name":"old","prefix":"nightly","tenant":""}}`) {
		t.Errorf("the operator was asked %s", op.seen())
	}

	for _, path := range []string{
		"/v1/clusters/" + dt.Cluster + "/bundle-sources",
		"/v1/clusters/" + dt.Cluster + "/bundle-sources/old/bundles",
	} {
		if code, _ := h.do(t, "GET", path, tom, ""); code != http.StatusForbidden {
			t.Errorf("tom reached %s: %d", path, code)
		}
	}
	if code, body := h.do(t, "GET", "/v1/tenants/demo/bundle-sources/destination/bundles", tom, ""); code != http.StatusOK {
		t.Errorf("tom's own tenant's source = %d %v", code, body)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/solo/bundle-sources", tom, ""); code != http.StatusForbidden {
		t.Errorf("tom reached another tenant's sources: %d", code)
	}
}

// A backup holds the tenant's people and data, so a member, who reads the
// tenant, does not get it: only whoever may take one.
func TestABackupIsDownloadedOnlyByTheTenantsAdministrator(t *testing.T) {
	op := &sourcingOperator{}
	h := startWith(t, op)
	path := "/v1/tenants/demo/backups/nightly/download"

	if code, _ := h.do(t, "GET", path, h.token(t, "tenant-demo", "mia"), ""); code != http.StatusForbidden {
		t.Errorf("a member's download = %d", code)
	}
	if strings.Contains(op.seen(), "STREAM") {
		t.Errorf("the operator was asked for the bundle: %s", op.seen())
	}
	if code, _ := h.do(t, "GET", path, h.token(t, "tenant-demo", "tom"), ""); code != http.StatusOK {
		t.Errorf("the administrator's download = %d", code)
	}
	if !strings.Contains(op.seen(), "STREAM") {
		t.Errorf("the operator was not asked for the bundle: %s", op.seen())
	}
}

// An import may name a bundle at a declared source; it goes on with the
// bundle the operator resolved, and restores that one.
func TestAnImportFromADeclaredSourceRestoresTheBundleItResolvedTo(t *testing.T) {
	defer api.SetImportPoll(20 * time.Millisecond)()
	op := &sourcingOperator{importingOperator: importingOperator{declared: true}}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")

	both := `{"bundle":{"bucket":"gentian-imports","prefix":"x"},"source":{"name":"old","prefix":"nightly"},"decryption":{"passphrase":"open sesame"}}`
	if code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, both); code != http.StatusBadRequest {
		t.Fatalf("a bundle and a source = %d %v", code, body)
	}
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice,
		`{"source":{"name":"old","prefix":"nightly"},"decryption":{"passphrase":"open sesame"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("import = %d %v", code, body)
	}
	if b, _ := body["bundle"].(map[string]any); b["endpoint"] != "https://s3.example" || b["prefix"] != "nightly" {
		t.Errorf("the import is of %v", body["bundle"])
	}
	waitImport(t, h, alice, "imported", "ready", "failed")
	if !strings.Contains(op.seen(), `"bundle":{"bucket":"old","prefix":"nightly","endpoint":"https://s3.example","credentialSecret":"bundle-source-old"}`) {
		t.Errorf("the restore was not of the resolved bundle: %s", op.seen())
	}
}

// A tenant's administrator fetches from the tenant's own sources under
// can_set_policy, and the operator is told whose fetch it is, so that it
// fetches only the tenant's bundles and counts the copy towards the tenant's
// limits; the fetch, the copy and an upload of the tenant's are read and
// made as the tenant's. The cluster's routes ask nothing of the kind.
func TestATenantsAdministratorFetchesFromItsOwnSourcesAsTheTenant(t *testing.T) {
	op := &sourcingOperator{}
	h := startWith(t, op)
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "POST", "/v1/tenants/demo/bundle-sources/destination/actions/fetch", tom, `{"prefix":"nightly"}`)
	if code != http.StatusAccepted || body["name"] != "bundle-fetch-1" {
		t.Fatalf("tom's fetch = %d %v", code, body)
	}
	if !strings.Contains(op.seen(), `/v1/bundle-fetches {"source":{"name":"destination","prefix":"nightly","tenant":"demo"},"tenant":"demo"}`) {
		t.Errorf("the operator was asked %s", op.seen())
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/bundle-sources/storage/actions/fetch", tom, `{"prefix":"nightly"}`); code != http.StatusForbidden {
		t.Errorf("tom fetched from another tenant's source: %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/demo/bundle-sources/storage/actions/fetch", tom, `{"prefix":"nightly"}`); code != http.StatusForbidden {
		t.Errorf("tom used the cluster's fetch: %d", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/bundle-fetches/bundle-fetch-1", tom, ""); code != http.StatusOK {
		t.Errorf("tom's fetch status = %d", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/bundles/20261016-000000-aaaaaaaa/download", tom, ""); code != http.StatusOK {
		t.Errorf("tom's copy = %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/bundles", tom, "tar"); code/100 != 2 {
		t.Errorf("tom's upload = %d", code)
	}
	if code, _ := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/bundles", alice, "tar"); code/100 != 2 {
		t.Errorf("alice's upload = %d", code)
	}
	if code, _ := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/bundle-fetches/bundle-fetch-1", alice, ""); code != http.StatusOK {
		t.Errorf("alice's fetch status = %d", code)
	}
	seen := op.seen()
	for _, want := range []string{
		"GET /v1/bundle-fetches/bundle-fetch-1?tenant=demo\n",
		"STREAM /v1/bundles/20261016-000000-aaaaaaaa/download?tenant=demo\n",
		"UPLOAD /v1/bundles?tenant=demo\n",
		"UPLOAD /v1/bundles\n",
		"GET /v1/bundle-fetches/bundle-fetch-1\n",
	} {
		if !strings.Contains(seen+"\n", want) {
			t.Errorf("the operator was not asked %q:\n%s", want, seen)
		}
	}
}
