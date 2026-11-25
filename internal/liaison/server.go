/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package liaison

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Two kinds of caller reach the liaison, on separate paths.
//
// A person, at the liaison's own address behind the tenant's sign-in: the
// front door hands every request under /connections/ and
// /api/v1/connections/ a token of that person made out to the liaison
// alone. No other path is published.
//
// The desktop's API, inside the tenant, under /api/v1/connections/ with the
// person's own token, the one the registrar and the usher accept too: a
// person manages their connections in the desktop's account panel. A sign-in
// started there comes back to the liaison's own address, as every sign-in
// does, and the person is then sent on to the desktop, at the address the
// platform told the liaison, never one a request names.
//
// The chaperone, inside the tenant, under /api/v1/internal/ with the key of
// the contract between the two: it tells the liaison which outside tool
// servers the tenant's agent bridges name, and asks for a short credential
// when it carries a call for a person.

// People says which person a token names.
type People interface {
	Verify(ctx context.Context, token string) (string, error)
}

// Server answers the liaison's routes.
type Server struct {
	Service *Service
	People  People
	// Edge says which person the token the desktop calls with names: the
	// person's own token, made out to the platform's edge audience. Nil, the
	// desktop is not accepted.
	Edge People
	// Host is the liaison's own host, where a sign-in comes back to. Empty,
	// it is the host the request came to, which is right only at the page.
	Host string
	// DesktopHost is where the desktop answers. Empty, a sign-in cannot be
	// started for the desktop.
	DesktopHost string
	// ChaperoneKey is what the chaperone presents. Empty, nothing is
	// accepted from it.
	ChaperoneKey string
	Log          *slog.Logger

	mux *http.ServeMux
}

// requestHeader has to be on every request of the page that changes
// something. A page of another site cannot set it without the browser
// asking first, and the liaison answers no such question.
const requestHeader = "X-Liaison-Request"

//go:embed page.html
var page string

// NewServer builds the routes.
func NewServer(service *Service, people People, chaperoneKey string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{Service: service, People: people, ChaperoneKey: chaperoneKey, Log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	s.mux.HandleFunc("GET /connections/{$}", s.person(s.showPage))
	s.mux.HandleFunc("GET /connections/callback", s.person(s.callback))
	s.mux.HandleFunc("GET /api/v1/connections/{$}", s.personOrDesktop(s.list))
	s.mux.HandleFunc("POST /api/v1/connections/{name}/connect", s.personOrDesktop(s.changing(s.connect)))
	s.mux.HandleFunc("POST /api/v1/connections/{name}/disconnect", s.personOrDesktop(s.changing(s.disconnect)))

	s.mux.HandleFunc("PUT /api/v1/internal/connections", s.chaperone(s.tell))
	s.mux.HandleFunc("POST /api/v1/internal/credential", s.chaperone(s.credential))
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func bearer(r *http.Request) string {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, detail string) {
	write(w, status, map[string]string{"detail": detail})
}

// caller is a person at the page: who they are, and the token that says so.
type caller struct {
	person string
	token  string
	// desktop: the token is the one the desktop calls with.
	desktop bool
}

// person accepts the token the front door hands the page: the person's,
// made out to the liaison.
func (s *Server) person(h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return s.verified(false, h)
}

// personOrDesktop accepts that token, or the person's own token the desktop
// calls with.
func (s *Server) personOrDesktop(h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return s.verified(true, h)
}

func (s *Server) verified(desktop bool, h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" || s.People == nil {
			fail(w, http.StatusUnauthorized, "Sign in to the workspace to manage your connections.")
			return
		}
		person, err := s.People.Verify(r.Context(), token)
		fromDesktop := false
		if err != nil && desktop && s.Edge != nil {
			person, err = s.Edge.Verify(r.Context(), token)
			fromDesktop = err == nil
		}
		if err != nil {
			fail(w, http.StatusUnauthorized, "Sign in to the workspace to manage your connections.")
			return
		}
		h(w, r, caller{person: person, token: token, desktop: fromDesktop})
	}
}

func (s *Server) changing(h func(http.ResponseWriter, *http.Request, caller)) func(http.ResponseWriter, *http.Request, caller) {
	return func(w http.ResponseWriter, r *http.Request, c caller) {
		if r.Header.Get(requestHeader) == "" {
			fail(w, http.StatusForbidden, "This request did not come from the connections page.")
			return
		}
		h(w, r, c)
	}
}

func (s *Server) chaperone(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		if s.ChaperoneKey == "" || key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.ChaperoneKey)) != 1 {
			fail(w, http.StatusUnauthorized, "The chaperone's key is needed here.")
			return
		}
		h(w, r)
	}
}

func (s *Server) showPage(w http.ResponseWriter, _ *http.Request, _ caller) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, page)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, c caller) {
	type row struct {
		Name      string `json:"name"`
		Service   string `json:"service"`
		Connected bool   `json:"connected"`
	}
	rows := []row{}
	for _, connection := range s.Service.Connections() {
		connected, err := s.Service.Connected(r.Context(), c.person, connection.Name)
		if err != nil {
			s.Log.ErrorContext(r.Context(), "the confidant could not be asked", "error", err.Error())
			fail(w, http.StatusBadGateway, "Your connections could not be read. Try again.")
			return
		}
		host := connection.URL
		if u, err := url.Parse(connection.URL); err == nil {
			host = u.Host
		}
		rows = append(rows, row{Name: connection.Name, Service: host, Connected: connected})
	}
	write(w, http.StatusOK, map[string]any{"connections": rows})
}

// redirect is the liaison's own address a sign-in comes back to: the host
// the front door routed this request by.
func (s *Server) redirect(r *http.Request) string {
	host := s.Host
	if host == "" {
		host = r.Host
	}
	return "https://" + host + "/connections/callback"
}

// desktopPage is the desktop's account panel at its Connections tab, with
// what happened.
func (s *Server) desktopPage(outcome string) string {
	return "https://" + s.DesktopHost + "/?account=connections&" + outcome
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request, c caller) {
	// The desktop's call: the person goes back to the desktop, which the
	// liaison can only send them to where the platform told it the desktop
	// is.
	if c.desktop && (s.DesktopHost == "" || s.Host == "") {
		fail(w, http.StatusServiceUnavailable, "This liaison does not know where the desktop is, so a sign-in cannot be started from it.")
		return
	}
	address, err := s.Service.Begin(r.Context(), c.person, r.PathValue("name"), s.redirect(r), c.desktop)
	if err != nil {
		s.Log.WarnContext(r.Context(), "a sign-in could not be started", "connection", r.PathValue("name"), "error", err.Error())
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.InfoContext(r.Context(), "connection", "action", "begin", "person", c.person, "connection", r.PathValue("name"))
	write(w, http.StatusOK, map[string]string{"url": address})
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	back := func(desktop bool, outcome string) {
		to := "/connections/?" + outcome
		if desktop && s.DesktopHost != "" {
			to = s.desktopPage(outcome)
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
	if q.Get("error") != "" {
		back(s.Service.Forget(c.person, q.Get("state")), "failed=declined")
		return
	}
	name, desktop, err := s.Service.Complete(r.Context(), c.person, c.token, q.Get("state"), q.Get("code"))
	if err != nil {
		s.Log.WarnContext(r.Context(), "a sign-in could not be completed", "person", c.person, "error", err.Error())
		back(desktop, "failed=1")
		return
	}
	s.Log.InfoContext(r.Context(), "connection", "action", "connected", "person", c.person, "connection", name)
	back(desktop, "connected="+url.QueryEscape(name))
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.Service.Disconnect(r.Context(), c.person, r.PathValue("name")); err != nil {
		s.Log.ErrorContext(r.Context(), "a connection could not be removed", "person", c.person, "error", err.Error())
		fail(w, http.StatusBadGateway, "The connection could not be removed. Try again.")
		return
	}
	s.Log.InfoContext(r.Context(), "connection", "action", "disconnected", "person", c.person, "connection", r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tell(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Connections []Connection `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		fail(w, http.StatusUnprocessableEntity, "Send {\"connections\": [...]}.")
		return
	}
	write(w, http.StatusOK, map[string]int{"connections": s.Service.Tell(body.Connections)})
}

func (s *Server) credential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Person     string `json:"person"`
		Connection string `json:"connection"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Person == "" || body.Connection == "" {
		fail(w, http.StatusUnprocessableEntity, "Send {\"person\": ..., \"connection\": ...}.")
		return
	}
	credential, expires, err := s.Service.Credential(r.Context(), body.Person, body.Connection)
	switch {
	case errors.Is(err, ErrNotConnected):
		s.Log.InfoContext(r.Context(), "connection", "action", "credential", "outcome", "not connected",
			"person", body.Person, "connection", body.Connection)
		fail(w, http.StatusConflict, s.notConnected())
		return
	case err != nil:
		s.Log.ErrorContext(r.Context(), "a credential could not be obtained", "person", body.Person,
			"connection", body.Connection, "error", err.Error())
		fail(w, http.StatusBadGateway, "The service could not be asked for a credential.")
		return
	}
	s.Log.InfoContext(r.Context(), "connection", "action", "credential", "outcome", "handed",
		"person", body.Person, "connection", body.Connection)
	write(w, http.StatusOK, map[string]any{"credential": credential, "expires_at": expires})
}

// notConnected says where a person who is not connected connects: the
// desktop's account panel, at its address where the liaison knows it.
func (s *Server) notConnected() string {
	if s.DesktopHost == "" {
		return "This person is not connected to the service: they connect in the desktop, under Account, Connections."
	}
	return "This person is not connected to the service: they connect in the desktop, under Account, Connections (" +
		"https://" + s.DesktopHost + "/?account=connections)."
}
