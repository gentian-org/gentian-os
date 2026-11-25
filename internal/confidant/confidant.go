/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package confidant keeps secrets that belong to one person each, for the
// components that act for that person (docs/security/secrets.md).
//
// A component that declares requires.services.personSecrets presents its own
// key. The key says which tenant and which component it is; nothing in a
// request does. Under the key a component reads, replaces and removes its
// own entries of the people of its tenant:
//
//	gentian-os/tenants/<tenant>/people/<person>/<component>/<name>
//
// The first entry a component keeps for a person needs a token of that
// person as well, the one the front door hands the component's page for
// them. So a component cannot open a space for somebody who never came to
// it. Once the person has, a renewed secret replaces the old one with the
// key alone, because that happens while the person is away. Removing every
// entry of a person closes the space again.
//
// The confidant is the only party with a vault role for these paths, and the
// role reaches nothing else. Every request it decides is written to the log
// with who asked, for whom, which entry and how it ended, and never a value.
package confidant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/guard"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// PersonTokenHeader carries the person's token on a write.
const PersonTokenHeader = "Gentian-Person-Token"

// issuedTo is the realm client a person's token must have been issued to:
// the one the front door exchanges a session for a component's own token
// with. A token the person obtained anywhere else is not the one a page of
// the component was handed for them.
const issuedTo = "gentian-edge-exchange"

// MaxValueBytes bounds one entry. A token or a password fits many times; a
// document does not belong here.
const MaxValueBytes = 16 << 10

// valueField is the one field of an entry in the vault.
const valueField = "value"

var (
	// personPattern is a person's id in the realm.
	personPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	// namePattern is an entry's name, chosen by the component.
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
)

// Keys is the list of the components that keep secrets of people, as the
// operator writes it.
type Keys = componentkeys.List[componentkeys.PersonSecrets]

// KeysList names the list in what is logged about it.
const KeysList = "the confidant's list of components"

// Vault is what the confidant asks of the vault.
type Vault interface {
	Get(ctx context.Context, logicalPath string) (map[string]string, error)
	Put(ctx context.Context, logicalPath string, data map[string]string) error
	DeleteTree(ctx context.Context, logicalPath string) error
	ListChildren(ctx context.Context, logicalPath string) ([]string, error)
	Request(ctx context.Context, method, path string, body any) (int, []byte, error)
}

// Tokens verifies a person's token for one audience.
type Tokens interface {
	VerifyFor(ctx context.Context, raw, audience string) (*authn.Identity, error)
}

// Config is what a Server is built from.
type Config struct {
	Keys   *Keys
	Tokens Tokens
	Vault  Vault
	Log    *slog.Logger
}

// Server answers the confidant's routes.
type Server struct {
	cfg Config
	mux *http.ServeMux
	// Addr is where Start listens.
	Addr string
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Keys == nil || cfg.Tokens == nil || cfg.Vault == nil {
		return nil, fmt.Errorf("the confidant needs its list of components, a token verifier and the vault")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), Addr: ":9446"}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.HandleFunc("PUT /v1/people/{person}/secrets/{name}", s.as("write", s.write))
	s.mux.HandleFunc("GET /v1/people/{person}/secrets/{name}", s.as("read", s.read))
	s.mux.HandleFunc("DELETE /v1/people/{person}/secrets/{name}", s.as("remove", s.remove))
	s.mux.HandleFunc("DELETE /v1/people/{person}", s.as("remove-all", s.removeAll))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, guard.Identify(w, r))
}

// Start serves until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{Addr: s.Addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	case err := <-done:
		return err
	}
}

// call is one admitted request: the component the key is of, and the entry
// asked about.
type call struct {
	holder *componentkeys.PersonSecrets
	person string
	name   string
	action string
}

func (c call) path() string {
	return secrets.PersonSecretPath(c.holder.Tenant, c.person, c.holder.Component, c.name)
}

func (c call) space() string {
	return secrets.PersonComponentPath(c.holder.Tenant, c.person, c.holder.Component)
}

// as admits a request by the component key it presents and the names in its
// path, and hands it on.
func (s *Server) as(action string, h func(http.ResponseWriter, *http.Request, call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		holder := s.cfg.Keys.Find(guard.Bearer(r))
		if holder == nil {
			s.cfg.Log.WarnContext(r.Context(), "person secret", "outcome", "refused", "reason", "no component holds this key",
				"action", action, "request_id", guard.ID(r.Context()))
			s.fail(w, http.StatusUnauthorized,
				"unauthorized: present the key of a component whose profile declares requires.services.personSecrets")
			return
		}
		c := call{holder: holder, person: r.PathValue("person"), name: r.PathValue("name"), action: action}
		if !personPattern.MatchString(c.person) || (action != "remove-all" && !namePattern.MatchString(c.name)) {
			s.record(r, c, "refused", "invalid name")
			s.fail(w, http.StatusBadRequest, "invalid name: a person is named by their id in the realm, "+
				"an entry by lower-case letters, digits, dots and hyphens, at most 63")
			return
		}
		h(w, r, c)
	}
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Value string `json:"value"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxValueBytes+1024))
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Value == "" || len(body.Value) > MaxValueBytes {
		s.record(r, c, "refused", "no value, or one too large")
		s.fail(w, http.StatusBadRequest, fmt.Sprintf(`send {"value": "..."} of at most %d bytes`, MaxValueBytes))
		return
	}
	if token := r.Header.Get(PersonTokenHeader); token != "" {
		// A token that is presented decides: one that does not hold is not
		// passed over in favor of the key alone.
		if reason := s.personSaysSo(r.Context(), c, token); reason != "" {
			s.record(r, c, "refused", reason)
			s.fail(w, http.StatusForbidden, "forbidden: "+reason)
			return
		}
	} else {
		open, err := s.cfg.Vault.ListChildren(r.Context(), c.space())
		if err != nil {
			s.vaultFailed(w, r, c, err)
			return
		}
		if len(open) == 0 {
			s.record(r, c, "refused", "first entry for this person without their token")
			s.fail(w, http.StatusForbidden, "forbidden: the first entry kept for a person needs that person's token in "+
				PersonTokenHeader+", the one the front door hands this component's page for them")
			return
		}
	}
	// One version is kept: the secret before a renewal is of no use to
	// anyone but a thief.
	status, answer, err := s.cfg.Vault.Request(r.Context(), http.MethodPost,
		secrets.Mount+"/metadata/"+c.path(), map[string]any{"max_versions": 1})
	if err == nil && status/100 != 2 {
		err = fmt.Errorf("set max_versions: HTTP %d: %.200s", status, answer)
	}
	if err == nil {
		err = s.cfg.Vault.Put(r.Context(), c.path(), map[string]string{valueField: body.Value})
	}
	if err != nil {
		s.vaultFailed(w, r, c, err)
		return
	}
	s.record(r, c, "done", "")
	w.WriteHeader(http.StatusNoContent)
}

// personSaysSo is "" where token is the person's own, handed to a page of
// this component, and otherwise why it is not.
func (s *Server) personSaysSo(ctx context.Context, c call, token string) string {
	ident, err := s.cfg.Tokens.VerifyFor(ctx, token, c.holder.Profile)
	switch {
	case err != nil:
		return "the person's token does not verify for this component"
	case ident.Client != issuedTo:
		return "the person's token is not the one the front door hands this component's page"
	case ident.Realm != c.holder.Realm:
		return "the person's token is of another realm than this tenant's"
	case ident.Subject != c.person:
		return "the person's token is of another person"
	}
	return ""
}

func (s *Server) read(w http.ResponseWriter, r *http.Request, c call) {
	data, err := s.cfg.Vault.Get(r.Context(), c.path())
	switch {
	case errors.Is(err, secrets.ErrNotFound) || (err == nil && data[valueField] == ""):
		s.record(r, c, "absent", "")
		s.fail(w, http.StatusNotFound, "nothing is kept under this name for this person")
		return
	case err != nil:
		s.vaultFailed(w, r, c, err)
		return
	}
	s.record(r, c, "done", "")
	w.Header().Set("Cache-Control", "no-store")
	s.json(w, http.StatusOK, map[string]string{valueField: data[valueField]})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, c call) {
	if err := s.cfg.Vault.DeleteTree(r.Context(), c.path()); err != nil {
		s.vaultFailed(w, r, c, err)
		return
	}
	s.record(r, c, "done", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeAll(w http.ResponseWriter, r *http.Request, c call) {
	if err := s.cfg.Vault.DeleteTree(r.Context(), c.space()); err != nil {
		s.vaultFailed(w, r, c, err)
		return
	}
	s.record(r, c, "done", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) vaultFailed(w http.ResponseWriter, r *http.Request, c call, err error) {
	s.cfg.Log.ErrorContext(r.Context(), "the vault did not answer a person secret request", "error", err.Error(),
		"action", c.action, "tenant", c.holder.Tenant, "component", c.holder.Component, "request_id", guard.ID(r.Context()))
	s.record(r, c, "failed", "the vault did not answer")
	s.fail(w, http.StatusBadGateway, "the vault did not answer; ask again")
}

// record writes one decided request to the log: who, for whom, which entry,
// how it ended. Never a value.
func (s *Server) record(r *http.Request, c call, outcome, reason string) {
	attrs := []any{
		"action", c.action, "outcome", outcome,
		"tenant", c.holder.Tenant, "component", c.holder.Component, "person", c.person,
		"request_id", guard.ID(r.Context()),
	}
	if c.name != "" {
		attrs = append(attrs, "name", c.name)
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	level := slog.LevelInfo
	if outcome == "refused" || outcome == "failed" {
		level = slog.LevelWarn
	}
	s.cfg.Log.Log(r.Context(), level, "person secret", attrs...)
}

func (s *Server) json(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) fail(w http.ResponseWriter, status int, message string) {
	s.json(w, status, map[string]string{"error": message})
}
