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
