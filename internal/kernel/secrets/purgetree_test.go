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

package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKV serves a small KV v2 tree and records what was deleted.
func fakeKV(t *testing.T, tree map[string][]string) (*KVClient, *[]string) {
	t.Helper()
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")
		switch r.Method {
		case http.MethodGet:
			keys, ok := tree[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		case http.MethodDelete:
			deleted = append(deleted, path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewKVClient(srv.URL, "role", "")
	c.SetStaticToken("test-token")
	return c, &deleted
}

// TestDeleteTreeRemovesEverythingBelow covers the case tenant deletion needs:
// the tenant's admin credential lives beside per-app subtrees, and only the
// latter were ever purged.
func TestDeleteTreeRemovesEverythingBelow(t *testing.T) {
	c, deleted := fakeKV(t, map[string][]string{
		"gentian-os/tenants/demo":                {"admin", "apps/"},
		"gentian-os/tenants/demo/apps":           {"nextcloud/"},
		"gentian-os/tenants/demo/apps/nextcloud": {"db"},
	})
	if err := c.DeleteTree(context.Background(), "gentian-os/tenants/demo"); err != nil {
		t.Fatalf("DeleteTree: %v", err)
	}
	joined := strings.Join(*deleted, " ")
	for _, want := range []string{
		"gentian-os/tenants/demo/admin",
		"gentian-os/tenants/demo/apps/nextcloud/db",
		"gentian-os/tenants/demo",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("did not delete %q; deleted: %v", want, *deleted)
		}
	}
}

// TestDeleteTreeRefusesEmptyPath is the safety property. An empty path would
// address the mount root, and this walks and deletes whatever it is given.
func TestDeleteTreeRefusesEmptyPath(t *testing.T) {
	c, deleted := fakeKV(t, map[string][]string{})
	for _, p := range []string{"", "/", "   "} {
		if err := c.DeleteTree(context.Background(), p); err == nil {
			t.Fatalf("DeleteTree(%q) was accepted; it addresses the whole mount", p)
		}
	}
	if len(*deleted) != 0 {
		t.Fatalf("nothing should have been deleted, got %v", *deleted)
	}
}
