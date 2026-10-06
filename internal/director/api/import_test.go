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
}

func (o *importingOperator) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case strings.HasSuffix(path, "/apps/status"):
		return http.StatusOK, []byte(`{"apps":[{"profile":"element","ready":true,"phase":"Ready"}]}`), nil
	case strings.Contains(path, "/restores/"):
		phase := "Running"
		if time.Now().After(o.restoreAt) {
			phase = "Ready"
		}
		return http.StatusOK, []byte(`{"name":"restore-1","phase":"` + phase + `","message":"1 app(s) restored","passwordResetRequired":true}`), nil
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
	switch {
	case path == "/v1/bundles/inspect":
		raw, _ := json.Marshal(body)
		if !strings.Contains(string(raw), `"passphrase":"open sesame"`) {
			return http.StatusBadRequest, []byte(`{"detail":"the manifest could not be opened with that key"}`), nil
		}
		return http.StatusOK, []byte(`{"bundle":{"bucket":"gentian-imports","prefix":"x"},"manifest":{"schemaVersion":1,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd","apps":[{"profile":"element"}]},"apps":[]}}`), nil
	case strings.HasSuffix(path, "/actions/restore"):
		o.restored++
		o.restoreAt = time.Now().Add(100 * time.Millisecond)
		return http.StatusAccepted, []byte(`{"name":"restore-1","tenant":"imported","phase":""}`), nil
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
			if st["passwordResetRequired"] != true || st["restore"] != "restore-1" {
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
