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

package credentialmgr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A KV v2 mount, reduced to the one property these tests are about: POST
// replaces the document, PATCH merges into it. Everything else — versions,
// metadata, cas — is absent because none of it distinguishes the two.
type fakeKV struct {
	doc     map[string]string
	methods []string
	patchCT string
}

func (k *fakeKV) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/metadata/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		k.methods = append(k.methods, r.Method)

		body, _ := io.ReadAll(r.Body)
		var in struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatalf("request body: %v", err)
		}

		switch r.Method {
		case http.MethodPatch:
			k.patchCT = r.Header.Get("Content-Type")
			if k.doc == nil {
				// No version to merge onto, which is what OpenBao answers 404 to.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			for key, v := range in.Data {
				k.doc[key] = v
			}
		case http.MethodPost:
			k.doc = map[string]string{}
			for key, v := range in.Data {
				k.doc[key] = v
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

func newKVBackedOpenBao(t *testing.T, kv *fakeKV) *OpenBao {
	t.Helper()
	srv := httptest.NewServer(kv.handler(t))
	t.Cleanup(srv.Close)
	return NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt"}, nil, false)
}

// TestWriteLeavesUnsuppliedKeysAlone is the whole reason Write patches.
//
// gentian-os/kernel/dns/cloudflare holds api-token beside zone-id and
// tunnel-cname, which are derived at install time and typed by nobody. A
// replacing write dropped both when an operator rotated the token through the
// console: the write succeeded, the console said so, and the ExternalSecret
// reading all three failed with `cannot find secret data for key: "zone-id"`,
// leaving the cluster serving the revoked token it already had.
func TestWriteLeavesUnsuppliedKeysAlone(t *testing.T) {
	kv := &fakeKV{doc: map[string]string{
		"api-token":    "old-token",
		"zone-id":      "03bed969",
		"tunnel-cname": "3bb57659.cfargotunnel.com",
	}}
	b := newKVBackedOpenBao(t, kv)

	if err := b.Write(context.Background(), "caller-token",
		"gentian-os/kernel/dns/cloudflare",
		map[string]string{"api-token": "new-token"}, "alice"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if kv.doc["api-token"] != "new-token" {
		t.Errorf("supplied key not written: %q", kv.doc["api-token"])
	}
	if kv.doc["zone-id"] != "03bed969" {
		t.Errorf("zone-id was lost: %q", kv.doc["zone-id"])
	}
	if kv.doc["tunnel-cname"] != "3bb57659.cfargotunnel.com" {
		t.Errorf("tunnel-cname was lost: %q", kv.doc["tunnel-cname"])
	}
}

// The merge has to happen on the server, because this client cannot read a
// value to merge one itself — see Metadata. That makes the content type part of
// the contract rather than a detail: without it a patch is not a merge patch.
func TestWriteSendsAMergePatch(t *testing.T) {
	kv := &fakeKV{doc: map[string]string{"a": "1"}}
	b := newKVBackedOpenBao(t, kv)

	if err := b.Write(context.Background(), "caller-token", "gentian-os/kernel/x",
		map[string]string{"a": "2"}, ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(kv.methods) == 0 || kv.methods[0] != http.MethodPatch {
		t.Fatalf("first request should be a PATCH, got %v", kv.methods)
	}
	if kv.patchCT != "application/merge-patch+json" {
		t.Fatalf("content type = %q", kv.patchCT)
	}
}

// A path nothing has seeded has no document to merge onto. Replacing it is then
// both correct and the only option, and loses nothing because there is nothing
// there — so the 404 falls back to a full write rather than failing.
func TestWriteCreatesAPathThatDoesNotExistYet(t *testing.T) {
	kv := &fakeKV{doc: nil}
	b := newKVBackedOpenBao(t, kv)

	if err := b.Write(context.Background(), "caller-token", "gentian-os/kernel/new",
		map[string]string{"password": "s3cret"}, "alice"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if kv.doc["password"] != "s3cret" {
		t.Fatalf("document not created: %#v", kv.doc)
	}
	if len(kv.methods) != 2 || kv.methods[0] != http.MethodPatch || kv.methods[1] != http.MethodPost {
		t.Fatalf("expected PATCH then POST, got %v", kv.methods)
	}
}
