/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func checkFixture(t *testing.T) (*fakeStore, http.Handler) {
	t.Helper()
	anna, err := authz.User("anna")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{allow: map[string]bool{
		anna + "|can_use|app:acme/projects":  true,
		anna + "|can_use|app:other/projects": true,
		anna + "|can_administer|tenant:acme": true,
	}}
	table := &Table{Checkers: []Checker{
		{Tenant: "acme", Component: "notary", KeyHash: keyHash("acme-key")},
		{Tenant: "globex", Component: "notary", KeyHash: keyHash("globex-key")},
	}}
	return store, New(Options{Store: store, Table: table}).CheckHandler()
}

func ask(h http.Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCheckAnswersForTheCheckersOwnTenant(t *testing.T) {
	_, h := checkFixture(t)
	if rec := ask(h, "acme-key", `{"person":"anna","app":"projects"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"allowed":true`) {
		t.Fatalf("anna may use acme/projects: got %d %s", rec.Code, rec.Body)
	}
	if rec := ask(h, "acme-key", `{"person":"ben","app":"projects"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"allowed":false`) {
		t.Fatalf("ben may not: got %d %s", rec.Code, rec.Body)
	}
	// The same person and app name asked by another tenant's checker is a
	// question about that tenant's app, which anna does not have.
	if rec := ask(h, "globex-key", `{"person":"anna","app":"projects"}`); !strings.Contains(rec.Body.String(), `"allowed":false`) {
		t.Fatalf("globex's checker asked about globex/projects: got %d %s", rec.Code, rec.Body)
	}
}

func TestCheckRefusesWhoeverHoldsNoKey(t *testing.T) {
	store, h := checkFixture(t)
	for _, key := range []string{"", "wrong", keyHash("acme-key")} {
		if rec := ask(h, key, `{"person":"anna","app":"projects"}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: got %d, want 401", key, rec.Code)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times for callers with no key", store.checks)
	}
}

func TestCheckCannotLeaveItsTenantOrItsOneQuestion(t *testing.T) {
	store, h := checkFixture(t)
	for _, body := range []string{
		`{"person":"anna","app":"../other/projects"}`,
		`{"person":"anna","app":"other/projects"}`,
		`{"person":"anna","app":""}`,
		`{"person":"","app":"projects"}`,
		`{"person":"anna","app":"projects","relation":"can_administer","object":"tenant:acme"`,
		`not json`,
	} {
		if rec := ask(h, "acme-key", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times for questions that are not the one question", store.checks)
	}
	// Fields a caller adds do not widen the question.
	rec := ask(h, "acme-key", `{"person":"anna","app":"projects","relation":"can_administer","object":"tenant:acme"}`)
	if rec.Code != 200 || store.checks != 1 {
		t.Fatalf("got %d, checks %d", rec.Code, store.checks)
	}
}

func TestCheckFailsClosedAndTakesNoOtherMethod(t *testing.T) {
	store, h := checkFixture(t)
	store.down = true
	if rec := ask(h, "acme-key", `{"person":"anna","app":"projects"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store down: got %d, want 503", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/check", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("GET answered")
	}
}

func TestTableRefusesAMalformedChecker(t *testing.T) {
	for name, entry := range map[string]string{
		"no tenant":   `{component: notary, keyHash: "` + keyHash("k") + `"}`,
		"no hash":     `{tenant: acme, component: notary}`,
		"a plain key": `{tenant: acme, component: notary, keyHash: "acme-key"}`,
	} {
		if _, err := ParseTable([]byte("checkers: [" + entry + "]")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	shared := `{tenant: acme, component: a, keyHash: "` + keyHash("k") + `"}, {tenant: globex, component: b, keyHash: "` + keyHash("k") + `"}`
	if _, err := ParseTable([]byte("checkers: [" + shared + "]")); err == nil {
		t.Error("two tenants sharing one key: accepted")
	}
	table, err := ParseTable([]byte(`checkers: [{tenant: acme, component: notary, keyHash: "` + keyHash("k") + `"}]`))
	if err != nil || table.checker("k") == nil || table.checker("other") != nil {
		t.Fatalf("a well-formed checker: %v", err)
	}
}
