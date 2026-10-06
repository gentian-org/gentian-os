/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// What a tenant is meant to have is declared in git by the director. The
// routes that once changed it here -- install, uninstall, the addon selection
// and the resource plan -- must stay gone: each was a second way to change
// declared state, with no author and no review.
func TestTheListenerHasNoWriteOfDeclaredState(t *testing.T) {
	h := &HTTPServer{Token: "director"}
	mux := h.routes()
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/tenants/demo/apps/nextcloud", ""},
		{"DELETE", "/v1/tenants/demo/apps/nextcloud", ""},
		{"PUT", "/v1/tenants/demo/apps/nextcloud/addons", `{"addons":[]}`},
		{"PUT", "/v1/tenants/demo/resources", `{"plan":"small"}`},
		{"PUT", "/v1/tenants/demo/backup-policy", `{}`},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer director")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d; the route must not exist", c.method, c.path, w.Code)
		}
	}
}

// guardOnly builds the token check around a handler that only says it was
// reached, so the check is tested without a cluster behind it.
func guardOnly(h *HTTPServer) *http.ServeMux {
	mux := http.NewServeMux()
	g := &guardedMux{mux: mux, auth: h.authenticated}
	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	g.Read("GET /v1/tenants/{tenant}/backups", reached)
	g.HandleFunc("GET /v1/tenants/{tenant}/backups/{name}/download", reached)
	g.HandleFunc("POST /v1/tenants/{tenant}/actions/delete-backup", reached)
	return mux
}

func answer(mux http.Handler, method, path, token string) int {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

// The usher's token reads state and does nothing else. It is a second secret
// so that the process facing every console's backend cannot delete a backup
// or purge an app, whatever happens to it.
func TestTheReadersTokenAdmitsReadsAndNothingElse(t *testing.T) {
	h := &HTTPServer{Token: "director"}
	h.SetReadToken("reader")
	mux := guardOnly(h)
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/v1/tenants/demo/backups", "director", http.StatusNoContent},
		{"GET", "/v1/tenants/demo/backups", "reader", http.StatusNoContent},
		{"GET", "/v1/tenants/demo/backups", "", http.StatusUnauthorized},
		{"GET", "/v1/tenants/demo/backups", "somebody", http.StatusUnauthorized},
		// A GET that was not registered as a read is the director's.
		{"GET", "/v1/tenants/demo/backups/b1/download", "director", http.StatusNoContent},
		{"GET", "/v1/tenants/demo/backups/b1/download", "reader", http.StatusForbidden},
		// No command, ever.
		{"POST", "/v1/tenants/demo/actions/delete-backup", "director", http.StatusNoContent},
		{"POST", "/v1/tenants/demo/actions/delete-backup", "reader", http.StatusForbidden},
		{"POST", "/v1/tenants/demo/actions/delete-backup", "", http.StatusUnauthorized},
	} {
		if got := answer(mux, c.method, c.path, c.token); got != c.want {
			t.Errorf("%s %s with %q answered %d, want %d", c.method, c.path, c.token, got, c.want)
		}
	}
}

// An unset token admits nobody, the caller who presents nothing included.
func TestAnUnsetTokenAdmitsNobody(t *testing.T) {
	// Neither configured: the listener says so.
	none := guardOnly(&HTTPServer{})
	for _, token := range []string{"", "director", "reader"} {
		if got := answer(none, "GET", "/v1/tenants/demo/backups", token); got != http.StatusServiceUnavailable {
			t.Errorf("no token configured, %q presented: %d, want 503", token, got)
		}
	}
	// Only the director's: there is no reader, and an empty bearer is not one.
	directorOnly := guardOnly(&HTTPServer{Token: "director"})
	for _, token := range []string{"", "reader"} {
		if got := answer(directorOnly, "GET", "/v1/tenants/demo/backups", token); got != http.StatusUnauthorized {
			t.Errorf("no reader configured, %q presented: %d, want 401", token, got)
		}
	}
	// Only the reader's: reads work, and nothing admits a command.
	h := &HTTPServer{}
	h.SetReadToken("reader")
	readerOnly := guardOnly(h)
	if got := answer(readerOnly, "GET", "/v1/tenants/demo/backups", "reader"); got != http.StatusNoContent {
		t.Errorf("reader's read: %d, want 204", got)
	}
	for _, token := range []string{"", "director"} {
		if got := answer(readerOnly, "POST", "/v1/tenants/demo/actions/delete-backup", token); got != http.StatusUnauthorized {
			t.Errorf("no director configured, %q presented a command: %d, want 401", token, got)
		}
	}
}

// Every route the reader's token admits is a GET, and the set is the one the
// usher serves: adding to it is a decision, so it is written down here.
func TestTheRoutesOpenToTheReaderAreTheUshersReads(t *testing.T) {
	h := &HTTPServer{Token: "director"}
	h.SetReadToken("reader")
	mux := h.routes()
	directorOnly := []struct{ method, path string }{
		{"GET", "/v1/tenants/demo"},
		{"GET", "/v1/tenants/demo/apps"},
		{"GET", "/v1/tenants/demo/backups/b1/download"},
		{"GET", "/v1/tenants/demo/restores/r1"},
		{"POST", "/v1/tenants/demo/actions/backup"},
		{"POST", "/v1/tenants/demo/actions/delete-backup"},
		{"POST", "/v1/tenants/demo/actions/restore"},
		{"POST", "/v1/tenants/demo/actions/notify"},
		{"POST", "/v1/tenants/demo/actions/purge-app"},
		{"POST", "/v1/tenants/demo/actions/provision-app"},
		{"POST", "/v1/bundles"},
		{"POST", "/v1/bundles/inspect"},
	}
	for _, c := range directorOnly {
		if got := answer(mux, c.method, c.path, "reader"); got != http.StatusForbidden {
			t.Errorf("%s %s with the reader's token answered %d, want 403", c.method, c.path, got)
		}
	}
}

func TestTheReadersTokenIsMintedOnceAndKept(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	first, err := ensureReadToken(ctx, c, c, "kernel-edge", "usher-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("token is %d characters, want 64 of hex", len(first))
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kernel-edge", Name: "usher-token"}, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[readTokenKey]) != first {
		t.Fatal("the Secret does not hold the token that was returned")
	}
	again, err := ensureReadToken(ctx, c, c, "kernel-edge", "usher-token")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatal("a second look minted a second token; every replica must admit the same one")
	}

	// A Secret that exists and holds nothing is filled, not replaced.
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kernel-edge", Name: "empty"}}
	if err := c.Create(ctx, empty); err != nil {
		t.Fatal(err)
	}
	filled, err := ensureReadToken(ctx, c, c, "kernel-edge", "empty")
	if err != nil || filled == "" || filled == first {
		t.Fatalf("an empty Secret was not given a token of its own: %q, %v", filled, err)
	}
}
