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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// LiteLLM teams, one per tenant.
//
// This was a shell loop: E-02 listed every Tenant, deleted a Job named
// litellm-teams-sync and applied a new one that called the same two endpoints.
// Per-tenant state converged by a script that runs when an operator happens to
// re-run the installer — so a tenant created afterwards had no team until
// somebody remembered, and two installs racing deleted each other's Job.
//
// The operator already speaks to this API for virtual keys, on the same base
// URL with the same master key. A team is the same shape and belongs beside it.

// ensureLiteLLMTeam creates the tenant's team if LiteLLM does not have one.
//
// Idempotent by asking first: /team/new on an existing alias is an error rather
// than a no-op, and the shell got away with it only because it deleted and
// recreated the Job each time.
func ensureLiteLLMTeam(ctx context.Context, masterKey, teamAlias string) error {
	exists, err := litellmTeamExists(ctx, masterKey, teamAlias)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	body, err := json.Marshal(map[string]any{"team_alias": teamAlias})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		litellmProxyBaseURL+"/team/new", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+masterKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LiteLLM /team/new: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("LiteLLM /team/new status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// litellmTeamExists reports whether a team with this alias is already known.
//
// /team/list rather than a lookup by alias: LiteLLM has no endpoint that takes
// one, and the list is small — one entry per tenant.
func litellmTeamExists(ctx context.Context, masterKey, teamAlias string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, litellmProxyBaseURL+"/team/list", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("LiteLLM /team/list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("LiteLLM /team/list status %d: %s", resp.StatusCode, string(respBody))
	}

	// The response is a list of team objects; only the alias matters here, and
	// LiteLLM has changed the envelope between versions, so decode loosely.
	var teams []map[string]any
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, &teams); err != nil {
		// An object, then — but only one that actually carries a teams array.
		// Decoding into a struct with a `teams` field would accept ANY object,
		// including LiteLLM's {"detail": ...} error bodies, and report a team
		// that exists as absent. The key has to be there.
		var wrapped map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return false, fmt.Errorf("LiteLLM /team/list: unrecognised response: %w", err)
		}
		inner, ok := wrapped["teams"]
		if !ok {
			return false, fmt.Errorf("LiteLLM /team/list: response has no team list: %s", truncate(raw, 200))
		}
		if err2 := json.Unmarshal(inner, &teams); err2 != nil {
			return false, fmt.Errorf("LiteLLM /team/list: teams is not a list: %w", err2)
		}
	}
	for _, t := range teams {
		if alias, _ := t["team_alias"].(string); alias == teamAlias {
			return true, nil
		}
	}
	return false, nil
}

// ensureTenantLiteLLMTeam is the reconciler's entry point.
//
// Never fatal. LiteLLM is an optional kernel service: a cluster that does not
// serve LLMs has no proxy to reach, and a tenant must not be held un-Ready
// because an optional component is absent or still starting. The next reconcile
// retries, which is what a controller is for and what the shell loop was not.
func (r *TenantReconciler) ensureTenantLiteLLMTeam(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	masterKey, err := r.getLiteLLMMasterKey(ctx)
	if err != nil {
		// No Secret means no LiteLLM on this cluster. Not a condition worth
		// logging on every reconcile of every tenant.
		return
	}
	if err := ensureLiteLLMTeam(ctx, masterKey, tenant.Name); err != nil {
		log.FromContext(ctx).V(1).Info("LiteLLM team sync deferred",
			"tenant", tenant.Name, "reason", err.Error())
	}
}

// truncate keeps an unexpected response body short enough to log.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
