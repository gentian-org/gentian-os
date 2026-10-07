/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
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

// Deleting a tenant with its data removes what the model gateway holds for
// it: the key of every app on record, and of every app it has now, and its
// team. With Retain, or without a gateway, nothing is touched; a gateway
// that does not answer fails the deletion.
func TestTenantDeleteRemovesTheModelKeysAndTheTeam(t *testing.T) {
	keys := map[string]bool{"demo-wiki": true, "demo-drive": true, "other-wiki": true}
	teams := map[string]string{"demo": "t-1", "other": "t-2"}
	down := false
	withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var body struct {
			KeyAliases []string `json:"key_aliases"`
			TeamIDs    []string `json:"team_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/key/list":
			out := []string{}
			if keys[r.URL.Query().Get("key_alias")] {
				out = append(out, "hash")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
		case "/key/delete":
			for _, a := range body.KeyAliases {
				delete(keys, a)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted_keys": body.KeyAliases})
		case "/team/list":
			out := []map[string]any{}
			for alias, id := range teams {
				out = append(out, map[string]any{"team_alias": alias, "team_id": id})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/team/delete":
			for _, id := range body.TeamIDs {
				for alias, have := range teams {
					if have == id {
						delete(teams, alias)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted_teams": body.TeamIDs})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	scheme := deleteGapsScheme()
	// wiki was uninstalled long ago and is on record; drive is installed.
	record := backup.NewProvisionedRecord("demo")
	if _, err := backup.RecordProvisioned(record, "wiki", backup.Provisioned{ModelKey: "demo-wiki"}); err != nil {
		t.Fatal(err)
	}
	masterKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: litellmMasterKeySecret, Namespace: litellmMasterKeyNS},
		Data:       map[string][]byte{litellmMasterKeySecKey: []byte("sk-master")},
	}
	tenant := planTenant("demo", "drive")
	ctx := context.Background()

	// No gateway on the cluster: nothing to do, and no error.
	bare := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(record.DeepCopy()).Build(), Scheme: scheme}
	tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := bare.deleteModelAccess(ctx, tenant); err != nil {
		t.Fatalf("no gateway: %v", err)
	}

	r := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(record, masterKey).Build(), Scheme: scheme}

	tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
	if err := r.deleteModelAccess(ctx, tenant); err != nil || len(keys) != 3 || len(teams) != 2 {
		t.Fatalf("deletionPolicy Retain: err = %v, keys = %v, teams = %v", err, keys, teams)
	}

	tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	down = true
	if err := r.deleteModelAccess(ctx, tenant); err == nil {
		t.Fatal("a gateway that does not answer was passed over")
	}
	down = false
	if err := r.deleteModelAccess(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if keys["demo-wiki"] || keys["demo-drive"] || !keys["other-wiki"] {
		t.Errorf("keys = %v, want the tenant's two gone and the other tenant's kept", keys)
	}
	if _, there := teams["demo"]; there || teams["other"] != "t-2" {
		t.Errorf("teams = %v", teams)
	}
	// Asked again, with nothing left: still no error.
	if err := r.deleteModelAccess(ctx, tenant); err != nil {
		t.Fatalf("a second pass: %v", err)
	}
}

// A key removed at a purge has to be registered again when the app comes
// back. The gateway goes on answering /key/info for a deleted key, so
// existence is asked of the list of keys under the alias.
func TestAModelKeyIsRegisteredAgainAfterItWasRemoved(t *testing.T) {
	listed, generated := false, 0
	withLiteLLM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/info":
			// What the gateway says of a deleted key.
			_, _ = w.Write([]byte(`{"key":"sk-gentian-demo-wiki","info":{}}`))
		case "/key/list":
			out := []string{}
			if listed {
				out = append(out, "hash")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
		case "/key/generate":
			generated++
			listed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	for i := 0; i < 2; i++ {
		if err := ensureLiteLLMVirtualKey(context.Background(), "sk-master", "sk-gentian-demo-wiki", "demo-wiki"); err != nil {
			t.Fatal(err)
		}
	}
	if generated != 1 {
		t.Errorf("the key was registered %d time(s), want once: not at all would leave the app a key that authenticates against nothing", generated)
	}
}
