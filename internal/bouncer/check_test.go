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
		// The platform tenant's people: root administers the cluster (by
		// group, as the account itself), audrey audits it and dana reads
		// acme's evidence (by name, as the account of the kernel realm).
		"user:root|can_audit|cluster:main":                 true,
		"user:root|can_configure|cluster:main":             true,
		"user:kernel/audrey|can_audit|cluster:main":        true,
		"user:kernel/audrey|can_read_evidence|tenant:acme": true,
		"user:kernel/dana|can_read_evidence|tenant:acme":   true,
	}}
	table := &Table{Checkers: []Checker{
		{Tenant: "acme", Component: "notary", KeyHash: keyHash("acme-key")},
		{Tenant: "globex", Component: "notary", KeyHash: keyHash("globex-key")},
		{Tenant: "platform", Component: "evidence", KeyHash: keyHash("platform-key"),
			Audit: &CheckerAudit{Cluster: "main", Realm: "kernel"}},
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
	// Fields a caller adds do not widen the question: an object is not
	// read, and a relation is an audit question, which this key may not ask.
	rec := ask(h, "acme-key", `{"person":"anna","app":"projects","object":"tenant:acme"}`)
	if rec.Code != 200 || store.checks != 1 {
		t.Fatalf("got %d, checks %d", rec.Code, store.checks)
	}
	for _, body := range []string{
		`{"person":"anna","app":"projects","relation":"can_administer","object":"tenant:acme"}`,
		`{"person":"anna","relation":"can_audit"}`,
		`{"person":"anna","relation":"can_read_evidence","tenant":"acme"}`,
	} {
		if rec := ask(h, "acme-key", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d %s, want 403", body, rec.Code, rec.Body)
		}
	}
	if store.checks != 1 {
		t.Fatalf("the store was asked an audit question for a key that may not ask one")
	}
}

// A key with Audit asks two questions more about a person of the platform
// tenant, as themselves and, refused, as the account of the kernel realm a
// role is held by name as: may they audit the cluster, and may they read one
// tenant's evidence.
func TestAnAuditKeyAsksWhoMayAuditAndReadEvidence(t *testing.T) {
	_, h := checkFixture(t)
	for body, want := range map[string]bool{
		`{"person":"root","relation":"can_audit"}`:                           true,
		`{"person":"audrey","relation":"can_audit"}`:                         true,
		`{"person":"dana","relation":"can_audit"}`:                           false,
		`{"person":"audrey","relation":"can_read_evidence","tenant":"acme"}`: true,
		`{"person":"dana","relation":"can_read_evidence","tenant":"acme"}`:   true,
		`{"person":"dana","relation":"can_read_evidence","tenant":"globex"}`: false,
		`{"person":"anna","relation":"can_read_evidence","tenant":"acme"}`:   false,
		// Configuring is the administrator's, never the auditor's.
		`{"person":"root","relation":"can_configure"}`:   true,
		`{"person":"audrey","relation":"can_configure"}`: false,
	} {
		rec := ask(h, "platform-key", body)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), `"allowed":true`) != want {
			t.Errorf("%s: got %d %s, want allowed %v", body, rec.Code, rec.Body, want)
		}
	}
}

func TestAnAuditKeyAsksNothingElse(t *testing.T) {
	store, h := checkFixture(t)
	for _, body := range []string{
		`{"person":"dana","relation":"can_administer","tenant":"acme"}`,
		`{"person":"dana","relation":"can_operate_system"}`,
		`{"person":"dana","relation":"can_configure","tenant":"acme"}`,
		`{"person":"dana","relation":"can_audit","tenant":"acme"}`,
		`{"person":"dana","relation":"can_read_evidence"}`,
		`{"person":"dana","relation":"can_read_evidence","tenant":"acme/x"}`,
		`{"person":"dana","relation":"can_read_evidence","tenant":"Acme"}`,
		`{"person":"dana","app":"projects","relation":"can_audit"}`,
		`{"person":"a/b","relation":"can_audit"}`,
	} {
		if rec := ask(h, "platform-key", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times for questions an audit key may not ask", store.checks)
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
		"no tenant":                          `{component: notary, keyHash: "` + keyHash("k") + `"}`,
		"no hash":                            `{tenant: acme, component: notary}`,
		"a plain key":                        `{tenant: acme, component: notary, keyHash: "acme-key"}`,
		"audit with no realm":                `{tenant: platform, component: evidence, keyHash: "` + keyHash("k") + `", audit: {cluster: main}}`,
		"audit with no cluster":              `{tenant: platform, component: evidence, keyHash: "` + keyHash("k") + `", audit: {realm: kernel}}`,
		"audit of a cluster that is no name": `{tenant: platform, component: evidence, keyHash: "` + keyHash("k") + `", audit: {cluster: "a:b", realm: kernel}}`,
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
