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

	"github.com/gentian-org/gentian-os/internal/audit"
)

// Checker is one component the operator lets ask a single question of the
// store: may this person use that app of the component's own tenant -- and,
// for a key with Audit, the two audit questions.
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
	// Audit, when set, lets the key ask two questions more about a person of
	// the platform tenant: may they audit the cluster, and may they read one
	// tenant's evidence. Nil for every key but the evidence service's and its
	// console's.
	Audit *CheckerAudit `json:"audit,omitempty"`
}

// CheckerAudit is what the audit questions are asked against: the cluster,
// and the realm whose accounts the people asked about are, the platform
// tenant's.
type CheckerAudit struct {
	Cluster string `json:"cluster"`
	Realm   string `json:"realm"`
}

// The audit questions a key with Audit may ask, beside the app question.
// can_configure is among them so that a store of evidence can keep its own
// settings -- what is kept and for how long -- to who configures the cluster,
// and its auditors to reading.
const (
	relationCanAudit        = "can_audit"
	relationCanReadEvidence = "can_read_evidence"
	relationCanConfigure    = "can_configure"
)

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
		if c.Audit != nil {
			if _, err := authz.HolderUser(c.Audit.Realm, "x"); err != nil || strings.TrimSpace(c.Audit.Cluster) == "" ||
				strings.ContainsAny(c.Audit.Cluster, ":#/ ") {
				return fmt.Errorf("route table: checker %d (%s/%s) asks the audit questions and needs a cluster and a realm", i, c.Tenant, c.Component)
			}
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
	// App is the profile name of an app of the checker's tenant: may the
	// person use it.
	App string `json:"app,omitempty"`
	// Relation, instead of App, asks an audit question of a key that may:
	// can_audit or can_configure, of the cluster, or can_read_evidence, of
	// Tenant.
	Relation string `json:"relation,omitempty"`
	Tenant   string `json:"tenant,omitempty"`
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

// question is what a checker asked, as the store is asked it.
type question struct {
	// holder is the role holder's account the person is asked as when
	// refused as themselves (authz.CheckAs); empty for the app question.
	user, holder     string
	relation, object string
	// tenant is the tenant the answer is about, for the record.
	tenant string
}

// questionOf reads the request as one question the checker may ask, or says
// why it is not, with the status to answer.
func questionOf(checker *Checker, req checkRequest) (question, int, string) {
	user, err := authz.User(req.Person)
	if err != nil {
		return question{}, http.StatusBadRequest, "person must be a subject"
	}
	if req.Relation == "" {
		if !appNamePattern.MatchString(req.App) || req.Tenant != "" {
			return question{}, http.StatusBadRequest, "app must be the name of an app of this tenant"
		}
		return question{user: user, relation: "can_use", object: authz.App(checker.Tenant, req.App), tenant: checker.Tenant}, 0, ""
	}
	if checker.Audit == nil {
		return question{}, http.StatusForbidden, "this key asks about the apps of its tenant only"
	}
	if req.App != "" {
		return question{}, http.StatusBadRequest, "ask about an app or a relation, not both"
	}
	// The audit questions are about people of the platform tenant, whose
	// cluster and tenant roles are held by name, as the account of its realm.
	holder, err := authz.HolderUser(checker.Audit.Realm, req.Person)
	if err != nil {
		return question{}, http.StatusBadRequest, "person must be a subject"
	}
	q := question{user: user, holder: holder, relation: req.Relation}
	switch req.Relation {
	case relationCanAudit, relationCanConfigure:
		if req.Tenant != "" {
			return question{}, http.StatusBadRequest, req.Relation + " is asked of the cluster, with no tenant"
		}
		q.object = authz.Cluster(checker.Audit.Cluster)
	case relationCanReadEvidence:
		if !appNamePattern.MatchString(req.Tenant) {
			return question{}, http.StatusBadRequest, "can_read_evidence needs the tenant's name"
		}
		q.object, q.tenant = authz.Tenant(req.Tenant), req.Tenant
	default:
		return question{}, http.StatusBadRequest, "relation must be can_audit, can_configure or can_read_evidence"
	}
	return q, 0, ""
}

func (d *Decider) check(w http.ResponseWriter, r *http.Request) {
	checker := d.Table().checker(bearer(r.Header.Get("Authorization")))
	if checker == nil {
		http.Error(w, "unknown key", http.StatusUnauthorized)
		return
	}
	var req checkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "the body must be JSON with person, and app or relation", http.StatusBadRequest)
		return
	}
	q, status, refusal := questionOf(checker, req)
	if status != 0 {
		http.Error(w, refusal, status)
		return
	}
	ok, err := authz.CheckAs(r.Context(), d.store, r.Header.Get("x-request-id"), q.user, q.holder, q.relation, q.object)
	if err != nil {
		d.log.WarnContext(r.Context(), "store unreachable; failing closed", "checker", checker.Component, "tenant", checker.Tenant, "error", err.Error())
		http.Error(w, "authorization store unreachable", http.StatusServiceUnavailable)
		return
	}
	outcome := audit.Refused
	if ok {
		outcome = audit.Allowed
	}
	// The person asked about is the subject: a component asked whether they
	// may use the app, audit, or read the evidence, and the answer is a
	// decision about them.
	audit.Write(r.Context(), d.log, audit.Record{
		Source: "bouncer", Kind: audit.KindDecision, RequestID: r.Header.Get("x-request-id"),
		Tenant: q.tenant, Subject: req.Person, Object: q.object, Action: q.relation, Outcome: outcome,
		Detail: map[string]any{"reason": "rights-check", "checker": checker.Component},
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(checkResponse{Allowed: ok})
}
