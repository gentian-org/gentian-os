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
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Checker is one component the operator lets ask a single question of the
// store: may this person use that app of the component's own tenant.
//
// It is for a component that acts for a person who is not at a browser, and
// so has no session this service could verify at the edge. Such a component
// would otherwise need the store's credential, which reads and writes
// everything. Here it holds a key of its own that is good for one question,
// about one tenant, and for nothing else: it cannot write, cannot list, and
// cannot name another tenant's app.
type Checker struct {
	// Tenant is the tenant whose apps the component may ask about.
	Tenant string `json:"tenant"`
	// Component names who is asking, for the log.
	Component string `json:"component"`
	// KeyHash is the SHA-256 of the key the component was given, in hex. The
	// key itself is in the component's Secret and nowhere else.
	KeyHash string `json:"keyHash"`
}

var keyHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// appNamePattern is a profile name: a DNS label. Holding the name to it is
// what keeps the question inside the checker's tenant, since the object is
// app:<tenant>/<name> and a name with a slash in it would be another's.
var appNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func validateCheckers(checkers []Checker) error {
	seen := map[string]bool{}
	for i, c := range checkers {
		if strings.TrimSpace(c.Tenant) == "" || strings.TrimSpace(c.Component) == "" {
			return fmt.Errorf("route table: checker %d needs tenant and component", i)
		}
		if !keyHashPattern.MatchString(c.KeyHash) {
			return fmt.Errorf("route table: checker %d (%s/%s) needs keyHash as 64 hex digits", i, c.Tenant, c.Component)
		}
		if seen[c.KeyHash] {
			return fmt.Errorf("route table: checker %d (%s/%s) shares its key with another", i, c.Tenant, c.Component)
		}
		seen[c.KeyHash] = true
	}
	return nil
}

// checker returns the checker a key belongs to, or nil.
func (t *Table) checker(key string) *Checker {
	if t == nil || key == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(key))
	got := []byte(hex.EncodeToString(sum[:]))
	var found *Checker
	// Every entry is compared, so the time taken says nothing about which
	// one matched or whether one did.
	for i := range t.Checkers {
		if subtle.ConstantTimeCompare(got, []byte(t.Checkers[i].KeyHash)) == 1 {
			found = &t.Checkers[i]
		}
	}
	return found
}

type checkRequest struct {
	// Person is the subject of the person asked about, as their tokens name
	// them.
	Person string `json:"person"`
	// App is the profile name of an app of the checker's tenant.
	App string `json:"app"`
}

type checkResponse struct {
	Allowed bool `json:"allowed"`
}

// CheckHandler answers a checker's question. It is served on a listener of
// its own, apart from the edge's and the health probe's, so that the network
// can admit components to it and to nothing else of this service.
func (d *Decider) CheckHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", d.check)
	return mux
}

func (d *Decider) check(w http.ResponseWriter, r *http.Request) {
	checker := d.Table().checker(bearer(r.Header.Get("Authorization")))
	if checker == nil {
		http.Error(w, "unknown key", http.StatusUnauthorized)
		return
	}
	var req checkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "the body must be JSON with person and app", http.StatusBadRequest)
		return
	}
	if !appNamePattern.MatchString(req.App) {
		http.Error(w, "app must be the name of an app of this tenant", http.StatusBadRequest)
		return
	}
	user, err := authz.User(req.Person)
	if err != nil {
		http.Error(w, "person must be a subject", http.StatusBadRequest)
		return
	}
	object := authz.App(checker.Tenant, req.App)
	ok, err := d.store.Check(r.Context(), r.Header.Get("x-request-id"), user, "can_use", object)
	if err != nil {
		d.log.WarnContext(r.Context(), "store unreachable; failing closed", "checker", checker.Component, "tenant", checker.Tenant, "error", err.Error())
		http.Error(w, "authorization store unreachable", http.StatusServiceUnavailable)
		return
	}
	d.log.InfoContext(r.Context(), "check", "checker", checker.Component, "tenant", checker.Tenant, "object", object, "allowed", ok)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(checkResponse{Allowed: ok})
}
