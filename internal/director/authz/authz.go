/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package authz asks OpenFGA whether a caller may do something, and records
// the answer.
//
// The director never decides: it names a relation and an object and OpenFGA
// answers (security principle 2). What this package adds is the two things a
// policy enforcement point owes the rest of the platform — every decision is
// logged with the request id that joins it to the issuer's event and to the
// commit it allowed (principle 7), and every identifier crosses into OpenFGA
// through one mapping, so the director, the gateway and the tuple writer can
// never disagree about what a user or a group is called.
package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/openfga"
)

// ErrInvalidID is returned for an identifier OpenFGA could not store.
var ErrInvalidID = errors.New("identifier cannot be represented in OpenFGA")

// Checker answers one authorization question.
type Checker interface {
	// Check reports whether user has relation on object. requestID is logged
	// with the decision and is not sent to OpenFGA.
	Check(ctx context.Context, requestID, user, relation, object string) (bool, error)
}

// Viewer reads who holds what on one object, for the console's read-only
// authorization view. Separate from Checker because it is a different
// question with a different audience: Check answers "may this caller do this,
// now", on the hot path; ViewOf answers "who holds what here", for a person
// reviewing it.
type Viewer interface {
	ViewOf(ctx context.Context, object string) (View, error)
}

// encode maps an external identifier onto OpenFGA's id alphabet. OpenFGA
// reserves ':' (type separator) and '#' (relation separator) and rejects
// whitespace. Keycloak uses ':' in group names (gentian:tenant:demo:admins)
// and in federated user ids (f:<provider>:<id>), so ':' becomes '/', which
// neither uses. Anything else that cannot be stored is refused, not repaired:
// two distinct names must never encode to the same id.
func encode(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "#/ \t\r\n") {
		return "", fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return strings.ReplaceAll(s, ":", "/"), nil
}

// User returns the OpenFGA user for a token subject.
func User(sub string) (string, error) {
	id, err := encode(sub)
	if err != nil {
		return "", err
	}
	return "user:" + id, nil
}

// Group returns the OpenFGA object for a Keycloak group name such as
// gentian:tenant:demo:admins.
func Group(name string) (string, error) {
	id, err := encode(name)
	if err != nil {
		return "", err
	}
	return "group:" + id, nil
}

// GroupFromPath returns the OpenFGA object for a Keycloak group path such as
// /gentian:tenant:demo:admins. Nested groups are not part of the platform's
// vocabulary and are refused.
func GroupFromPath(path string) (string, error) {
	return Group(strings.TrimPrefix(path, "/"))
}

// Tenant and Cluster name the objects the director
// checks against. Tenant and cluster names are DNS labels and need no
// encoding; they are validated where they enter.
func Tenant(name string) string  { return "tenant:" + name }
func Cluster(name string) string { return "cluster:" + name }

// OpenFGA is a Checker backed by an OpenFGA server.
type OpenFGA struct {
	fga     *openfga.Client
	storeID string
	// modelID pins the authorization model. A check against "whatever is
	// latest" changes meaning when a model is written, which is not something
	// a decision log can explain afterwards.
	modelID string
	log     *slog.Logger
	now     func() time.Time
}

// Options configures an OpenFGA checker.
type Options struct {
	BaseURL  string
	APIToken string
	StoreID  string
	ModelID  string
	Logger   *slog.Logger
	Client   *http.Client
	Now      func() time.Time
}

// NewOpenFGA returns a checker. The store id is required; the model id is
// required too, for the reason on the field.
func NewOpenFGA(o Options) (*OpenFGA, error) {
	if o.BaseURL == "" || o.StoreID == "" {
		return nil, errors.New("authz: OpenFGA URL and store id are required")
	}
	if o.ModelID == "" {
		return nil, errors.New("authz: authorization model id is required: decisions must be made against a pinned model")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &OpenFGA{
		fga:     openfga.New(o.BaseURL, o.APIToken, o.Client),
		storeID: o.StoreID, modelID: o.ModelID,
		log: o.Logger, now: o.Now,
	}, nil
}

// Check implements Checker.
//
// An error is a denial. The caller gets (false, err) and must not proceed;
// there is no mode in which an unreachable OpenFGA lets a write through.
func (c *OpenFGA) Check(ctx context.Context, requestID, user, relation, object string) (allowed bool, err error) {
	start := c.now()
	defer func() {
		attrs := []any{
			"request_id", requestID, "user", user, "relation", relation, "object", object,
			"allowed", allowed, "model", c.modelID, "duration_ms", c.now().Sub(start).Milliseconds(),
		}
		if err != nil {
			c.log.ErrorContext(ctx, "authz decision failed", append(attrs, "error", err.Error())...)
			return
		}
		c.log.InfoContext(ctx, "authz decision", attrs...)
	}()
	return c.fga.Check(ctx, c.storeID, c.modelID, user, relation, object)
}

// Question is one authorization question: does User have Relation on Object.
type Question struct {
	User, Relation, Object string
}

// BatchChecker answers several questions in one round trip, in the order
// asked. Like Check, an error is a denial of all of them.
type BatchChecker interface {
	BatchCheck(ctx context.Context, requestID string, questions []Question) ([]bool, error)
}

// CheckAll answers questions in the order asked: in batches where c can
// answer them so, else one at a time. Either way an error answers nothing,
// and the caller must refuse as it would on one failed Check.
func CheckAll(ctx context.Context, c Checker, requestID string, questions []Question) ([]bool, error) {
	if b, ok := c.(BatchChecker); ok {
		return b.BatchCheck(ctx, requestID, questions)
	}
	out := make([]bool, len(questions))
	for i, q := range questions {
		ok, err := c.Check(ctx, requestID, q.User, q.Relation, q.Object)
		if err != nil {
			return nil, err
		}
		out[i] = ok
	}
	return out, nil
}

// BatchCheck implements BatchChecker: the questions in requests of at most
// openfga.MaxBatchCheck, each decision logged as Check logs it.
func (c *OpenFGA) BatchCheck(ctx context.Context, requestID string, questions []Question) ([]bool, error) {
	out := make([]bool, 0, len(questions))
	for len(questions) > 0 {
		n := min(len(questions), openfga.MaxBatchCheck)
		chunk := questions[:n]
		questions = questions[n:]
		tuples := make([]openfga.Tuple, n)
		for i, q := range chunk {
			tuples[i] = openfga.Tuple{User: q.User, Relation: q.Relation, Object: q.Object}
		}
		start := c.now()
		allowed, err := c.fga.BatchCheck(ctx, c.storeID, c.modelID, tuples)
		ms := c.now().Sub(start).Milliseconds()
		if err != nil {
			c.log.ErrorContext(ctx, "authz decision failed",
				"request_id", requestID, "checks", n, "model", c.modelID, "duration_ms", ms, "error", err.Error())
			return nil, err
		}
		for i, q := range chunk {
			c.log.InfoContext(ctx, "authz decision",
				"request_id", requestID, "user", q.User, "relation", q.Relation, "object", q.Object,
				"allowed", allowed[i], "model", c.modelID, "duration_ms", ms)
		}
		out = append(out, allowed...)
	}
	return out, nil
}

// Tuple is one stored relationship.
type Tuple = openfga.Tuple

// Read returns the stored tuples matching a filter. Any field may be empty,
// within what OpenFGA accepts: an object may be a bare type ("group:") when a
// user is given. Pages are followed to the end.
func (c *OpenFGA) Read(ctx context.Context, filter Tuple) ([]Tuple, error) {
	return c.fga.Read(ctx, c.storeID, openfga.ReadOptions{Filter: &filter, PageSize: 100})
}

// Write adds and removes tuples. Each request is atomic; a change larger than
// one request is split, deletes first, so that an interruption leaves less
// access rather than more.
func (c *OpenFGA) Write(ctx context.Context, writes, deletes []Tuple) error {
	return c.fga.Write(ctx, c.storeID, writes, deletes, openfga.WriteOptions{Model: c.modelID})
}

// isNotFound reports OpenFGA saying that what was asked for does not exist.
func isNotFound(err error) bool { return openfga.IsStatus(err, http.StatusNotFound) }

// TenantRelations are the tenant verbs a person is told they hold, by the
// usher's /me, so that a console shows what they may do and hides the rest.
var TenantRelations = []string{
	"can_enter", "can_view", "can_administer", "can_install_app", "can_manage_users",
	"can_set_policy", "can_grant", "can_approve_privilege", "can_expose",
}

// ClusterRelations are the cluster verbs a person is told they hold: the ones
// that decide which screens a console shows them. Both lists are permissions
// of the model, which verify-authz-vocabulary holds them to.
var ClusterRelations = []string{
	"can_configure", "can_deploy_tenant", "can_operate_system", "can_install_shared",
	"can_grant_shared", "can_approve", "can_approve_perimeter", "can_set_admission", "can_edit_raw", "can_audit",
}

// Holds answers which of relations the caller holds on object, as the
// account or as the realm's role holder (CheckAs), all asked together. An
// error answers none of them.
func Holds(ctx context.Context, c Checker, requestID, user, holder, object string, relations []string) (map[string]bool, error) {
	questions := make([]Question, 0, 2*len(relations))
	for _, rel := range relations {
		questions = append(questions, Question{User: user, Relation: rel, Object: object})
		if holder != "" {
			questions = append(questions, Question{User: holder, Relation: rel, Object: object})
		}
	}
	allowed, err := CheckAll(ctx, c, requestID, questions)
	if err != nil {
		return nil, err
	}
	per := 1
	if holder != "" {
		per = 2
	}
	out := make(map[string]bool, len(relations))
	for i, rel := range relations {
		out[rel] = slices.Contains(allowed[i*per:(i+1)*per], true)
	}
	return out, nil
}
