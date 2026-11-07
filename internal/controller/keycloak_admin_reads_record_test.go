/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeKeycloakUsers is a realm of 205 people: a login that is an address,
// every seventh one switched off, every eleventh one with an address only in
// its email field, and the kernel realm's "administrator" with its address in
// email.
func fakeKeycloakUsers() []map[string]any {
	var users []map[string]any
	for i := range 204 {
		u := map[string]any{
			"id":       fmt.Sprintf("id-%03d", i),
			"username": fmt.Sprintf("p%03d@demo.example", i),
			"email":    "",
			"enabled":  i%7 != 3,
		}
		if i%11 == 5 {
			u["username"] = fmt.Sprintf("login%03d", i)
			u["email"] = fmt.Sprintf("Mail%03d@demo.example", i)
		}
		users = append(users, u)
	}
	users = append(users, map[string]any{"id": "id-admin", "username": "administrator", "email": "admin@demo.example", "enabled": true})
	return users
}

// fakeKeycloak answers the token and user-listing calls the mail readers
// make, and records each request.
type fakeKeycloak struct {
	mu        sync.Mutex
	calls     []string
	tokens    int
	tokenCode int
	usersCode int
}

func (f *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/realms/master/protocol/openid-connect/token" {
		_ = r.ParseForm()
		f.calls = append(f.calls, fmt.Sprintf("POST token client_id=%q client_secret=%q grant_type=%s",
			r.PostForm.Get("client_id"), r.PostForm.Get("client_secret"), r.PostForm.Get("grant_type")))
		if f.tokenCode != 0 {
			w.WriteHeader(f.tokenCode)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client"}`))
			return
		}
		f.tokens++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("token-%d", f.tokens), "expires_in": 300})
		return
	}
	f.calls = append(f.calls, fmt.Sprintf("%s %s auth=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization")))
	if f.usersCode != 0 {
		w.WriteHeader(f.usersCode)
		_, _ = w.Write([]byte(`{"error":"unknown_error"}`))
		return
	}
	var first, maxN int
	_, _ = fmt.Sscan(r.URL.Query().Get("first"), &first)
	_, _ = fmt.Sscan(r.URL.Query().Get("max"), &maxN)
	all := fakeKeycloakUsers()
	page := []map[string]any{}
	for i := first; i < len(all) && i < first+maxN; i++ {
		page = append(page, all[i])
	}
	_ = json.NewEncoder(w).Encode(page)
}

// describeNames is a list by its length, a digest and its ends, so a change
// in what is listed shows without the transcript holding 200 names.
func describeNames(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return fmt.Sprintf("%d names sha=%x first=%s last=%s", len(names), sum[:6], names[0], names[len(names)-1])
}

// What the mail reconciler reads from Keycloak, and how it asks: held to
// testdata/keycloak-admin-reads.txt.
func TestKeycloakAdminReadsTranscript(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	type step struct {
		name string
		run  func(r *TenantReconciler) string
	}
	steps := []step{
		{"realm users", func(r *TenantReconciler) string {
			got, err := r.keycloakRealmUsers(context.Background(), "demo")
			return fmt.Sprintf("err=%v %s", err, describeNames(got))
		}},
		{"realm addresses", func(r *TenantReconciler) string {
			got, err := r.keycloakRealmAddresses(context.Background(), "demo")
			has := func(a string) bool { i := sort.SearchStrings(got, a); return i < len(got) && got[i] == a }
			return fmt.Sprintf("err=%v %s admin=%v off=%v email-only=%v", err, describeNames(got),
				has("admin@demo.example"), has("p003@demo.example"), has("Mail005@demo.example"))
		}},
		{"realm holders", func(r *TenantReconciler) string {
			got, err := r.keycloakRealmHolders(context.Background(), "demo")
			var names []string
			for a := range got {
				names = append(names, a)
			}
			sort.Strings(names)
			return fmt.Sprintf("err=%v %s off=%v email-only-lowered=%v", err, describeNames(names),
				got["p003@demo.example"], got["mail005@demo.example"])
		}},
	}

	type setup struct {
		name      string
		secret    map[string]string
		tokenCode int
		usersCode int
	}
	setups := []setup{
		{name: "answers", secret: map[string]string{"client_id": "gentian-admin", "client_secret": "s3cret"}},
		{name: "credentials with a trailing newline", secret: map[string]string{"client_id": "gentian-admin\n", "client_secret": "s3cret\n"}},
		{name: "token refused", secret: map[string]string{"client_id": "gentian-admin", "client_secret": "s3cret"}, tokenCode: http.StatusUnauthorized},
		{name: "listing refused", secret: map[string]string{"client_id": "gentian-admin", "client_secret": "s3cret"}, usersCode: http.StatusInternalServerError},
		{name: "secret incomplete", secret: map[string]string{"client_id": "gentian-admin"}},
		{name: "no secret"},
	}

	var b strings.Builder
	for _, s := range setups {
		for _, st := range steps {
			kc := &fakeKeycloak{tokenCode: s.tokenCode, usersCode: s.usersCode}
			srv := httptest.NewServer(kc)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if s.secret != nil {
				data := map[string][]byte{"url": []byte(srv.URL + "/")}
				for k, v := range s.secret {
					data[k] = []byte(v)
				}
				builder = builder.WithObjects(&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: keycloakAdminSecret, Namespace: identityNamespace},
					Data:       data,
				})
			}
			r := &TenantReconciler{Client: builder.Build()}
			result := strings.ReplaceAll(st.run(r), srv.URL, "<keycloak>")
			srv.Close()
			fmt.Fprintf(&b, "%s, %s: %s\n", s.name, st.name, result)
			for _, c := range kc.calls {
				fmt.Fprintf(&b, "  %s\n", c)
			}
		}
	}
	holdToTranscript(t, *updateTranscripts, "keycloak-admin-reads", b.String())
}
