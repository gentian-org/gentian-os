/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withLiteLLM points the package's proxy URL at a stub for the duration of a test.
func withLiteLLM(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	prev := litellmProxyBaseURL
	litellmProxyBaseURL = srv.URL
	t.Cleanup(func() {
		litellmProxyBaseURL = prev
		srv.Close()
	})
}

// LiteLLM has shipped /team/list as both a bare array and an object wrapping
// one. Getting this wrong is silent: an unrecognised envelope reads as "no
// teams", and /team/new then fails on every reconcile for a tenant that already
// has a team.
func TestLiteLLMTeamExistsAcceptsBothEnvelopes(t *testing.T) {
	for name, body := range map[string]string{
		"bare array": `[{"team_alias":"acme","team_id":"t-1"}]`,
		"wrapped":    `{"teams":[{"team_alias":"acme","team_id":"t-1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/team/list" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer sk-master" {
					t.Errorf("Authorization = %q", got)
				}
				_, _ = w.Write([]byte(body))
			}))

			got, err := litellmTeamExists(context.Background(), "sk-master", "acme")
			if err != nil {
				t.Fatalf("litellmTeamExists: %v", err)
			}
			if !got {
				t.Fatal("existing team reported as absent")
			}

			got, err = litellmTeamExists(context.Background(), "sk-master", "other")
			if err != nil {
				t.Fatalf("litellmTeamExists: %v", err)
			}
			if got {
				t.Fatal("absent team reported as present")
			}
		})
	}
}

// An envelope neither decode understands must be an error, not a false "no
// teams" that sends /team/new at a team that exists.
func TestLiteLLMTeamExistsRejectsUnknownEnvelope(t *testing.T) {
	withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"detail":{"error":"no teams"}}`))
	}))

	if _, err := litellmTeamExists(context.Background(), "sk-master", "acme"); err == nil {
		t.Fatal("expected an error for an unrecognised /team/list response")
	}
}

// The whole point of asking first: /team/new on an existing alias is an error
// in LiteLLM, so a second reconcile of the same tenant must not call it.
func TestEnsureLiteLLMTeamIsIdempotent(t *testing.T) {
	created := 0
	withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/team/list":
			if created == 0 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"team_alias":"acme","team_id":"t-1"}]`))
		case "/team/new":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode /team/new body: %v", err)
			}
			if body["team_alias"] != "acme" {
				t.Errorf("team_alias = %v, want acme", body["team_alias"])
			}
			created++
			_, _ = w.Write([]byte(`{"team_id":"t-1"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))

	for i := 0; i < 3; i++ {
		if err := ensureLiteLLMTeam(context.Background(), "sk-master", "acme"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("/team/new called %d times, want 1", created)
	}
}

// A proxy that is up but unhappy must surface as an error the reconciler can
// retry, not as a silent success.
func TestEnsureLiteLLMTeamSurfacesFailure(t *testing.T) {
	withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/team/list" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	}))

	err := ensureLiteLLMTeam(context.Background(), "sk-master", "acme")
	if err == nil {
		t.Fatal("expected an error when /team/new fails")
	}
}
