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
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
	"github.com/gentian-org/gentian-os/internal/placement"
)

// LiteLLM teams, one per tenant.
//
// A tenant's team is what its keys belong to: it names the models the
// tenant's apps may use, as the access groups of the gateway's configuration,
// and carries the tenant's budget (Tenant.spec.llm). Every key of the tenant
// names the same models (model_access_reconciler.go), so a request is held to
// them by the key and by the team.

// tenantTeam is what the tenant's team carries: the models every tenant may
// use and those of the providers the tenant opted in to, without the groups
// that hold no model on this cluster, and the tenant's budget.
//
// A claim that cannot be read leaves the groups unfiltered: a group grants
// only the models the gateway's configuration puts in it, so the filter only
// keeps the gateway from listing a group's name as a model.
func (r *TenantReconciler) tenantTeam(ctx context.Context, tenant *gentianov1alpha1.Tenant) modelgateway.Team {
	var providers []string
	var budget *gentianov1alpha1.TenantLLMBudget
	if tenant.Spec.LLM != nil {
		providers, budget = tenant.Spec.LLM.Providers, tenant.Spec.LLM.Budget
	}
	var served map[string]bool
	if claim, found, err := readClusterModels(ctx, r.Client); err != nil {
		log.FromContext(ctx).V(1).Info("the Cluster claim's models cannot be read; the team names every group it may",
			"tenant", tenant.Name, "reason", err.Error())
	} else if found {
		served = claim.ServedGroups()
	}
	team := modelgateway.Team{Models: modelgateway.TeamModels(providers, served)}
	if budget != nil {
		team.MaxBudget, team.BudgetDuration, team.RPMLimit, team.TPMLimit =
			budget.MaxBudget, budget.BudgetDuration, budget.RPMLimit, budget.TPMLimit
	}
	return team
}

// readClusterModels reads spec.llm of the cluster's claim. found is false,
// with no error, where there is no claim or its kind is not installed.
func readClusterModels(ctx context.Context, c client.Reader) (modelgateway.ClaimModels, bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := c.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return modelgateway.ClaimModels{}, false, nil
		}
		return modelgateway.ClaimModels{}, false, err
	}
	if len(list.Items) == 0 {
		return modelgateway.ClaimModels{}, false, nil
	}
	sort.Slice(list.Items, func(a, b int) bool { return list.Items[a].GetName() < list.Items[b].GetName() })
	return claimModels(&list.Items[0]), true, nil
}

func claimModels(u *unstructured.Unstructured) modelgateway.ClaimModels {
	var out modelgateway.ClaimModels
	raw, found, _ := unstructured.NestedMap(u.Object, "spec", "llm")
	if !found {
		return out
	}
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// ensureTenantTeam makes the tenant's team carry what tenantTeam says, and
// answers its id.
func (r *TenantReconciler) ensureTenantTeam(ctx context.Context, gateway *modelgateway.Client, tenant *gentianov1alpha1.Tenant) (string, error) {
	return gateway.EnsureTeam(ctx, modelgateway.TeamAlias(tenant.Name), r.tenantTeam(ctx, tenant))
}

// ensureTenantLiteLLMTeam is the reconciler's entry point for a tenant whose
// apps hold no key: their team still follows the tenant's settings.
//
// Never fatal. LiteLLM is an optional kernel service: a cluster that does not
// serve LLMs has no proxy to reach, and a tenant must not be held un-Ready
// because an optional component is absent or still starting. The next reconcile
// retries.
func (r *TenantReconciler) ensureTenantLiteLLMTeam(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	masterKey, err := r.getLiteLLMMasterKey(ctx)
	if err != nil {
		// No Secret means no LiteLLM on this cluster. Not a condition worth
		// logging on every reconcile of every tenant.
		return
	}
	if _, err := r.ensureTenantTeam(ctx, litellmGateway(masterKey), tenant); err != nil {
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
	// since the record was last written. Not the components the platform
	// places: their key is on record before it is registered
	// (ensureModelAccess), so the record above has it.
	installed, err := placement.Installed(ctx, r.Client, tenant.Spec.Apps)
	if err != nil {
		return fmt.Errorf("remove the model keys of tenant %s: %w", tenant.Name, err)
	}
	for _, app := range installed {
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
