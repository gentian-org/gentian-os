/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package porter

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed web
var web embed.FS

// Server is the porter's pages and what they ask of it.
//
// It is served behind the edge's session and nowhere else. The edge hands
// every request the person's token made out to the porter (the entry says
// exchangeToken) and their id in a header; a request the edge did not pass
// does not reach the pods. What the pages ask goes to /api/, which takes a
// request only from the porter's own pages.
//
// The dialog is a window of its own. The page of an app that opened it is
// known by its origin, which the browser states with the page's message and
// the app cannot choose; the plan says which origins are an app's that may
// ask, and for what. What the person picked is told to that origin and no
// other.
type Server struct {
	// Plan reads the plan as it is now.
	Plan  func() (*Plan, error)
	Homes *Homes
	Log   *slog.Logger
}

// Handler is the porter's routes.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(web, "web")
	files := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /dialog", func(w http.ResponseWriter, r *http.Request) {
		page(w)
		r.URL.Path = "/dialog.html"
		files.ServeHTTP(w, r)
	})
	for _, asset := range []string{"/dialog.js", "/dialog.css"} {
		mux.HandleFunc("GET "+asset, func(w http.ResponseWriter, r *http.Request) {
			page(w)
			files.ServeHTTP(w, r)
		})
	}
	mux.HandleFunc("GET /api/ask", s.own(s.ask))
	mux.HandleFunc("GET /api/list", s.own(s.list))
	mux.HandleFunc("POST /api/pick", s.own(s.pick))
	return mux
}

// page sets what every page of the porter's is served with: nothing but its
// own files runs in it, and it is shown in no frame.
func page(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// own takes a request only from the porter's own pages: one a browser marks
// as coming from the same origin. A page of another site, an app of the same
// tenant included, cannot send that, and neither can a navigation.
func (s *Server) own(next func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			s.fail(w, r, refuse(http.StatusForbidden, "This is asked by the file dialog only."))
			return
		}
		if err := next(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		refusal = &Refusal{Status: http.StatusBadGateway, Message: "Your files could not be reached. Try again in a moment."}
	}
	s.Log.Warn("refused", "path", r.URL.Path, "status", refusal.Status, "person", person(r), "error", err.Error())
	answer(w, refusal.Status, map[string]string{"error": refusal.Message})
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// edgeToken is the person's token the edge put on the request.
func edgeToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// person is the person's id as the edge states it, for the log. Nothing is
// decided by it: what a request may see is the home's answer to the token.
func person(r *http.Request) string { return r.Header.Get("X-Gentian-Subject") }

// asked is the app whose page asks, by the origin the dialog's page read
// from the browser, where the plan lets it ask for the use.
func (s *Server) asked(origin string, use Use) (*Plan, *App, error) {
	plan, err := s.Plan()
	if err != nil {
		return nil, nil, err
	}
	app := plan.AppOf(origin, use)
	if app == nil {
		return nil, nil, refuse(http.StatusForbidden, "This app may not ask for your files.")
	}
	if plan.Home == nil {
		return nil, nil, refuse(http.StatusServiceUnavailable, "This workspace has no place for files.")
	}
	return plan, app, nil
}

// ask says whether the page at the origin may ask for the use, before the
// person is shown anything.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) error {
	_, app, err := s.asked(r.URL.Query().Get("origin"), Use(r.URL.Query().Get("use")))
	if err != nil {
		return err
	}
	answer(w, http.StatusOK, map[string]string{"app": app.Profile})
	return nil
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) error {
	plan, err := s.Plan()
	if err != nil {
		return err
	}
	p := r.URL.Query().Get("path")
	entries, err := s.Homes.List(r.Context(), plan.Home, edgeToken(r), p)
	if err != nil {
		return err
	}
	answer(w, http.StatusOK, map[string]any{"path": p, "entries": entries})
	return nil
}

func (s *Server) pick(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Origin string `json:"origin"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		return refuse(http.StatusBadRequest, "That is no request of the file dialog's.")
	}
	plan, app, err := s.asked(body.Origin, Pick)
	if err != nil {
		return err
	}
	picked, err := s.Homes.Pick(r.Context(), plan.Home, edgeToken(r), body.Path)
	if err != nil {
		return err
	}
	s.Log.Info("picked", "person", person(r), "app", app.Profile, "origin", body.Origin, "file", picked.ID)
	answer(w, http.StatusOK, picked)
	return nil
}
