/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/guard/guardtest"
)

// Each caller has an allowance of their own: a person by their account, a
// component by its key. A caller over it is answered 429 before the store is
// asked anything, and the next caller is not affected.
func TestEachCallerHasItsOwnAllowance(t *testing.T) {
	ids := guardtest.Authn{
		"ada":       {Subject: "ada", Realm: "gentian"},
		"ada-again": {Subject: "ada", Realm: "gentian", Client: "console"},
		"una":       {Subject: "u1", Realm: "demo"},
		// The same subject in another realm is another account.
		"ada-elsewhere": {Subject: "ada", Realm: "demo"},
	}
	checks := &guardtest.Checks{Held: map[string]bool{
		"user:ada can_view tenant:demo":      true,
		"user:demo/u1 can_view tenant:demo":  true,
		"user:demo/ada can_view tenant:demo": true,
	}}
	keys := &CommissionKeys{}
	keys.Set([]CommissionKey{{Key: componentkeys.Key{Tenant: "home", Component: "partners", KeyHash: componentkeys.Hash("the-key")}, Tenants: []string{"demo"}}})
	s := &Server{
		cfg:    Config{Authn: ids, Authz: checks, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Cluster: "c1", CommissionKeys: keys},
		mux:    http.NewServeMux(),
		limits: newCallerLimits(CallerLimit{PerSecond: 1, Burst: 3}),
	}
	ok := func(w http.ResponseWriter, _ *http.Request, _ call) { w.WriteHeader(http.StatusNoContent) }
	s.identified("GET /me", ok)
	s.guarded("GET /tenants/{t}", "can_view", tenantObject, ok)
	s.commissioned("GET /tenants/{t}/partners", "can_view", tenantObject, ok)

	call := func(path, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}

	// The burst, across routes and tokens of the one account.
	for i, path := range []string{"/me", "/tenants/demo", "/me"} {
		token := "ada"
		if i == 2 {
			token = "ada-again"
		}
		if w := call(path, token); w.Code != http.StatusNoContent {
			t.Fatalf("request %d of the burst answered %d", i+1, w.Code)
		}
	}
	checks.Take()
	w := call("/tenants/demo", "ada")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("over the allowance: %d, Retry-After %q; want 429 and 1", w.Code, w.Header().Get("Retry-After"))
	}
	if asked := checks.Take(); len(asked) != 0 {
		t.Errorf("a caller over the allowance was asked about in the store: %v", asked)
	}
	if w := call("/me", "ada-again"); w.Code != http.StatusTooManyRequests {
		t.Errorf("another token of the same account answered %d; the allowance is the account's", w.Code)
	}

	// Others are untouched.
	for _, token := range []string{"una", "ada-elsewhere"} {
		if w := call("/tenants/demo", token); w.Code != http.StatusNoContent {
			t.Errorf("%s answered %d after another caller used up theirs", token, w.Code)
		}
	}
	// Nobody is refused as unauthenticated, not counted.
	if w := call("/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a request without a token answered %d, want 401", w.Code)
	}

	// A component's key has an allowance of its own, apart from people's.
	for i := 0; i < 3; i++ {
		if w := call("/tenants/demo/partners", "the-key"); w.Code != http.StatusNoContent {
			t.Fatalf("the key's request %d answered %d", i+1, w.Code)
		}
	}
	if w := call("/tenants/demo/partners", "the-key"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Errorf("the key over its allowance answered %d", w.Code)
	}
}

// Without a limit configured, nobody is counted.
func TestNoLimitCountsNobody(t *testing.T) {
	none := newCallerLimits(CallerLimit{})
	if none != nil {
		t.Fatal("a zero limit made buckets")
	}
	for i := 0; i < 1000; i++ {
		if !none.allow("user:gentian/ada") {
			t.Fatal("refused without a limit")
		}
	}
}

// The defaults, the director's environment, and 0 for none.
func TestTheCallerLimitIsTheDefaultsUnlessTheEnvironmentSays(t *testing.T) {
	t.Setenv("DIRECTOR_REQUESTS_PER_SECOND", "")
	t.Setenv("DIRECTOR_REQUEST_BURST", "")
	if got := CallerLimitFromEnv(); got != (CallerLimit{PerSecond: DefaultCallerRequestsPerSecond, Burst: DefaultCallerRequestBurst}) {
		t.Errorf("defaults = %+v", got)
	}
	t.Setenv("DIRECTOR_REQUESTS_PER_SECOND", "50")
	t.Setenv("DIRECTOR_REQUEST_BURST", "20")
	if got := CallerLimitFromEnv(); got != (CallerLimit{PerSecond: 50, Burst: 50}) {
		t.Errorf("a burst below the rate is raised to it: %+v", got)
	}
	t.Setenv("DIRECTOR_REQUESTS_PER_SECOND", "lots")
	t.Setenv("DIRECTOR_REQUEST_BURST", "")
	if got := CallerLimitFromEnv(); got.PerSecond != DefaultCallerRequestsPerSecond {
		t.Errorf("an unusable value replaced the default: %+v", got)
	}
	t.Setenv("DIRECTOR_REQUESTS_PER_SECOND", "0")
	if got := CallerLimitFromEnv(); got != (CallerLimit{}) {
		t.Errorf("0 turns the limit off: %+v", got)
	}
}

// Callers whose allowance is whole again are forgotten once many are
// remembered, and a caller in the middle of a burst is not.
func TestIdleCallersAreForgotten(t *testing.T) {
	c := newCallerLimits(CallerLimit{PerSecond: 1, Burst: 2})
	c.allow("busy")
	c.allow("busy")
	for i := 0; len(c.byCaller) < maxIdleCallers; i++ {
		c.byCaller[fmt.Sprintf("idle-%d", i)] = rate.NewLimiter(1, 2)
	}
	c.allow("new")
	if _, ok := c.byCaller["busy"]; !ok {
		t.Error("a caller who used their allowance was forgotten, and would start whole again")
	}
	if len(c.byCaller) != 2 {
		t.Errorf("%d callers remembered, want the busy one and the new one", len(c.byCaller))
	}
	if c.allow("busy") {
		t.Error("the busy caller's allowance was reset")
	}
}
