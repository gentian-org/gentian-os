/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/audit"
	"github.com/gentian-org/gentian-os/internal/tokenreview"
)

// The dispatcher's second listener: the acts the director asks for.
//
// An act is done once, on a call, and follows nothing that is declared: a
// backup taken now, a backup deleted. What brings the cluster in line with
// what is declared is the operator's, whoever asks for it, and is not here.
//
// The listener is apart from the one components call (server.go), on a port
// of its own, and admits one caller: the director, by a token of its
// ServiceAccount issued for this listener, which the API server is asked
// about. The person an act is for is the name the director passes, which it
// verified; it is recorded and decides nothing here.

// ActsPort is the listener the director asks for acts at.
const ActsPort = int32(8081)

// ActorHeader carries the person an act is for, as the director verified it.
const ActorHeader = "X-Gentian-Actor"

// Identifier says whose a token is: tokenreview.Cache.
type Identifier interface {
	Ready() bool
	Identify(ctx context.Context, token string) (string, error)
}

// ErrNoSuch is something an act names that is not there.
var ErrNoSuch = errors.New("not found")

// ErrExists is something an act would make that is there already.
var ErrExists = errors.New("already exists")

// Backup is one backup as the director asks for it.
type Backup struct {
	Name        string                              `json:"name"`
	Apps        []string                            `json:"apps"`
	Recipients  []string                            `json:"recipients"`
	Destination *gentianov1alpha1.ExportDestination `json:"destination"`
}

// Exports writes the record a backup is taken from. The work is the
// operator's, which reads the record.
type Exports interface {
	// Start writes the record of one backup of a tenant, asked for by actor.
	Start(ctx context.Context, tenant string, backup Backup, actor string) error
	// Delete removes the record of one backup of a tenant.
	Delete(ctx context.Context, tenant, name string) error
}

// Acts serves the director's acts.
type Acts struct {
	exports Exports
	review  Identifier
	// director is the username of the director's ServiceAccount.
	director string
	log      *slog.Logger
	now      func() time.Time
}

// NewActs returns the listener's handler. An empty director, or a review
// that cannot ask, refuses every request.
func NewActs(exports Exports, review Identifier, director string, log *slog.Logger) *Acts {
	return &Acts{exports: exports, review: review, director: director, log: log, now: time.Now}
}

// started is what an act answers: what was started, and the name to ask
// about it by.
type started struct {
	Action string `json:"action"`
	Tenant string `json:"tenant"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func actsAnswer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func actsRefuse(w http.ResponseWriter, status int, message string) {
	actsAnswer(w, status, map[string]string{"detail": message})
}

// Handler is the listener.
func (a *Acts) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/backup", a.asDirector(a.startBackup))
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/delete-backup", a.asDirector(a.deleteBackup))
	return mux
}

// asDirector lets a request through only with a token the API server says is
// the director's, issued for this listener.
func (a *Acts) asDirector(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.director == "" || a.review == nil || !a.review.Ready() {
			actsRefuse(w, http.StatusServiceUnavailable, "this listener has no caller configured and refuses every request")
			return
		}
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token = strings.TrimSpace(token); !bearer || token == "" {
			actsRefuse(w, http.StatusUnauthorized, "this listener is the director's; present a ServiceAccount token of its")
			return
		}
		username, err := a.review.Identify(r.Context(), token)
		switch {
		case errors.Is(err, tokenreview.ErrNotAuthenticated):
			actsRefuse(w, http.StatusUnauthorized, "this listener is the director's; the token presented is not a valid one for it")
		case err != nil:
			// Refused, not admitted: a listener that opens when the API
			// server does not answer is no listener for one caller.
			a.log.ErrorContext(r.Context(), "a caller could not be identified and was refused", "error", err.Error())
			actsRefuse(w, http.StatusServiceUnavailable, "the caller could not be identified: the API server did not answer")
		case username != a.director:
			actsRefuse(w, http.StatusForbidden, "this listener is the director's; this identity is not it")
		default:
			next(w, r)
		}
	}
}

func (a *Acts) record(r *http.Request, action, tenant, object, outcome string, err error) {
	rec := audit.Record{
		Source: "dispatcher", Kind: audit.KindChange, RequestID: r.Header.Get("X-Request-Id"),
		Tenant: tenant, Subject: r.Header.Get(ActorHeader), Object: object, Action: action, Outcome: outcome,
	}
	if err != nil {
		rec.Detail = map[string]any{"reason": err.Error()}
	}
	audit.Write(r.Context(), a.log, rec)
}

// failed answers an act that did not happen, by what kept it.
func (a *Acts) failed(w http.ResponseWriter, r *http.Request, action, tenant, object string, err error) {
	a.record(r, action, tenant, object, audit.Failed, err)
	switch {
	case errors.Is(err, ErrNoSuch):
		actsRefuse(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrExists):
		actsRefuse(w, http.StatusBadRequest, err.Error())
	default:
		actsRefuse(w, http.StatusBadGateway, err.Error())
	}
}

func (a *Acts) startBackup(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	var backup Backup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&backup); err != nil && !errors.Is(err, io.EOF) {
		actsRefuse(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if backup.Name == "" {
		backup.Name = "manual-" + a.now().UTC().Format("20060102-150405")
	}
	if !dnsLabel.MatchString(backup.Name) {
		actsRefuse(w, http.StatusBadRequest, fmt.Sprintf("%q is not a name a backup can have", backup.Name))
		return
	}
	if err := a.exports.Start(r.Context(), tenant, backup, r.Header.Get(ActorHeader)); err != nil {
		a.failed(w, r, "backup", tenant, backup.Name, err)
		return
	}
	a.record(r, "backup", tenant, backup.Name, audit.OK, nil)
	actsAnswer(w, http.StatusAccepted, started{Action: "backup", Tenant: tenant, Name: backup.Name, Status: "started"})
}

func (a *Acts) deleteBackup(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Name == "" {
		actsRefuse(w, http.StatusBadRequest, `the body must be {"name": "<backup>"}`)
		return
	}
	if err := a.exports.Delete(r.Context(), tenant, body.Name); err != nil {
		a.failed(w, r, "delete-backup", tenant, body.Name, err)
		return
	}
	a.record(r, "delete-backup", tenant, body.Name, audit.OK, nil)
	actsAnswer(w, http.StatusAccepted, started{Action: "delete-backup", Tenant: tenant, Name: body.Name, Status: "started"})
}
