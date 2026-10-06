/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
