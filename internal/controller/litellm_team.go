/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
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
// one, and the list is small — one entry per tenant. The reading of it is the
// model gateway package's, which removing a team uses too.
func litellmTeamExists(ctx context.Context, masterKey, teamAlias string) (bool, error) {
	_, found, err := litellmGateway(masterKey).TeamID(ctx, teamAlias)
	return found, err
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
	if err := ensureLiteLLMTeam(ctx, masterKey, modelgateway.TeamAlias(tenant.Name)); err != nil {
		log.FromContext(ctx).V(1).Info("LiteLLM team sync deferred",
			"tenant", tenant.Name, "reason", err.Error())
	}
}

// deleteModelAccess removes what the model gateway holds for a tenant that is
// being deleted with its data: the key of every app on record as having one,
// and the tenant's team.
//
// Neither was ever removed. A key registered for an app went on
// authenticating after the app, and the tenant, were gone -- and the key is
// made from the tenant's and the app's names, so whoever knew those knew it.
//
// With deletionPolicy Retain both stay, as everything does. A cluster with no
// gateway has neither: the admin key's Secret is what says there is one. A
// gateway that is there and does not answer fails the pass, and the deletion
// comes back to it.
func (r *TenantReconciler) deleteModelAccess(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	masterKey, err := r.getLiteLLMMasterKey(ctx)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove the model keys of tenant %s: %w", tenant.Name, err)
	}
	gateway := litellmGateway(masterKey)

	recorded, err := r.provisionedStores(ctx, tenant.Name)
	if err != nil {
		return err
	}
	aliases := map[string]struct{}{}
	for _, p := range recorded {
		if p.ModelKey != "" {
			aliases[p.ModelKey] = struct{}{}
		}
	}
	// And the apps the tenant has now, whose key may have been registered
	// since the record was last written.
	for _, app := range tenant.Spec.Apps {
		aliases[modelgateway.KeyAlias(tenant.Name, app.Profile)] = struct{}{}
	}
	for alias := range aliases {
		if _, err := gateway.DeleteKey(ctx, alias); err != nil {
			return fmt.Errorf("remove the model key %s of tenant %s: %w", alias, tenant.Name, err)
		}
	}
	if _, err := gateway.DeleteTeam(ctx, modelgateway.TeamAlias(tenant.Name)); err != nil {
		return fmt.Errorf("remove the model gateway's team of tenant %s: %w", tenant.Name, err)
	}
	return nil
}
