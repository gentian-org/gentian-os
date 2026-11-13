/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Server is the chaperone's HTTP API. A part that is nil is a question
// nobody answers: the routes that need it answer 503.
type Server struct {
	// Service is nil until the rights check, the adapters and the journal
	// are all bound.
	Service *Service
	// Passes verifies an engine's pass; Persons a present person's token.
	Passes  Passes
	Persons Persons
	Log     *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

func (s *Server) now() int64 {
	if s.Now != nil {
		return s.Now().Unix()
	}
	return time.Now().Unix()
}

// Handler serves the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// What is bound. A chaperone with a question unanswered refuses, by
	// design, so this is always 200.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]bool{
			"service": s.Service != nil,
			"passes":  s.Passes != nil,
			"persons": s.Persons != nil && s.Service != nil && s.Service.Credentials != nil,
		})
	})
	// An engine, with a pass.
	mux.HandleFunc("GET /api/v1/tools", s.withPass(func(w http.ResponseWriter, r *http.Request, pass Pass) {
		tools, err := s.Service.Offered(r.Context(), pass)
		s.answerList(w, tools, err, false)
	}))
	mux.HandleFunc("POST /api/v1/tools/call", s.withPass(func(w http.ResponseWriter, r *http.Request, pass Pass) {
		call, ok := readCall(w, r)
		if !ok {
			return
		}
		result, err := s.Service.Call(r.Context(), pass, call.Tool, call.Arguments, s.now())
		s.answerCall(w, result, err)
	}))
	// A program a person connected: the call carries that person's own token.
	mux.HandleFunc("GET /api/v1/present/tools", s.withPerson(func(w http.ResponseWriter, r *http.Request, who Present) {
		tools, err := s.Service.OfferedPresent(r.Context(), who)
		s.answerList(w, tools, err, true)
	}))
	mux.HandleFunc("POST /api/v1/present/tools/call", s.withPerson(func(w http.ResponseWriter, r *http.Request, who Present) {
		call, ok := readCall(w, r)
		if !ok {
			return
		}
		result, err := s.Service.CallPresent(r.Context(), who, call.Tool, call.Arguments, s.now())
		s.answerCall(w, result, err)
	}))
	return mux
}

func (s *Server) bound(w http.ResponseWriter) bool {
	if s.Service == nil {
		detail(w, http.StatusServiceUnavailable,
			"The chaperone is not connected to the rights check, the adapters or its journal.")
		return false
	}
	return true
}

func (s *Server) withPass(next func(http.ResponseWriter, *http.Request, Pass)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.bound(w) {
			return
		}
		if s.Passes == nil {
			detail(w, http.StatusServiceUnavailable, "The chaperone has no issuer to verify passes against.")
			return
		}
		token := bearer(r)
		if token == "" {
			detail(w, http.StatusUnauthorized, "No pass")
			return
		}
		pass, err := s.Passes.Verify(r.Context(), token, s.now())
		if err != nil {
			detail(w, http.StatusUnauthorized, err.Error())
			return
		}
		next(w, r, pass)
	}
}

func (s *Server) withPerson(next func(http.ResponseWriter, *http.Request, Present)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.bound(w) {
			return
		}
		if s.Persons == nil || s.Service.Credentials == nil {
			detail(w, http.StatusServiceUnavailable, "The chaperone is not connected to the tenant's realm.")
			return
		}
		token := bearer(r)
		if token == "" {
			detail(w, http.StatusUnauthorized, "No token")
			return
		}
		who, err := s.Persons.Verify(r.Context(), token, s.now())
		if err != nil {
			detail(w, http.StatusUnauthorized, err.Error())
			return
		}
		next(w, r, who)
	}
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

type callBody struct {
	// Tool is "<adapter>/<tool>@<version>".
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func readCall(w http.ResponseWriter, r *http.Request) (callBody, bool) {
	var call callBody
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	// A number is carried as it was written, not as a float.
	decoder.UseNumber()
	if err := decoder.Decode(&call); err != nil || call.Tool == "" {
		detail(w, http.StatusUnprocessableEntity, "A call names a tool and gives its arguments as an object.")
		return call, false
	}
	return call, true
}

func (s *Server) answerList(w http.ResponseWriter, tools []Tool, err error, schema bool) {
	if err != nil {
		s.failed(w, err)
		return
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		entry := map[string]any{"tool": t.Key(), "app": t.App, "class": t.Class, "description": t.Description}
		if schema {
			entry["inputSchema"] = t.InputSchema
		}
		out = append(out, entry)
	}
	write(w, http.StatusOK, out)
}

func (s *Server) answerCall(w http.ResponseWriter, result any, err error) {
	if err != nil {
		s.failed(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) failed(w http.ResponseWriter, err error) {
	var refused *Refused
	var adapter *AdapterError
	switch {
	case errors.Is(err, ErrRightsUnavailable):
		if s.Log != nil {
			s.Log.Warn("the rights check could not be asked", "error", err.Error())
		}
		detail(w, http.StatusServiceUnavailable, ErrRightsUnavailable.Error())
	case errors.As(err, &refused):
		write(w, http.StatusForbidden, map[string]any{"detail": map[string]string{"reason": refused.Reason}})
	case errors.As(err, &adapter):
		write(w, http.StatusBadGateway, map[string]any{"detail": map[string]string{"reason": adapter.Message}})
	default:
		if s.Log != nil {
			s.Log.Error("a call failed", "error", err.Error())
		}
		detail(w, http.StatusInternalServerError, "The chaperone failed.")
	}
}

func detail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]string{"detail": message})
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
