/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package resourceplan resolves which ResourcePlan a tenant is on, which plans
// it may move to, and whether a move is safe to make.
//
// Nothing here writes. Selecting a plan is a GitOps edit performed by
// internal/applifecycle; this package decides what that edit is allowed to be.
package resourceplan

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Catalogue is the cluster's ResourcePlan set, ordered by tier.
type Catalogue struct {
	Plans []gentianov1alpha1.ResourcePlan
}

// Load reads every ResourcePlan in the cluster, smallest tier first.
func Load(ctx context.Context, c client.Client) (*Catalogue, error) {
	var list gentianov1alpha1.ResourcePlanList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list resource plans: %w", err)
	}
	plans := list.Items
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Spec.Tier != plans[j].Spec.Tier {
			return plans[i].Spec.Tier < plans[j].Spec.Tier
		}
		return plans[i].Name < plans[j].Name
	})
	return &Catalogue{Plans: plans}, nil
}

// Get returns the named plan, or nil.
func (c *Catalogue) Get(name string) *gentianov1alpha1.ResourcePlan {
	if c == nil || name == "" {
		return nil
	}
	for i := range c.Plans {
		if c.Plans[i].Name == name {
			return &c.Plans[i]
		}
	}
	return nil
}

// Default returns the lowest-tier plan marked default, or nil when none is.
//
// Lowest rather than first-found: several plans setting the flag is a
// misconfiguration, and of the two ways to resolve it, assuming a tenant is on
// a bigger plan than they chose is the one that overcharges them.
func (c *Catalogue) Default() *gentianov1alpha1.ResourcePlan {
	if c == nil {
		return nil
	}
	for i := range c.Plans {
		if c.Plans[i].Spec.Default {
			return &c.Plans[i]
		}
	}
	return nil
}

// Match returns the plan whose quotas equal the given ones, or nil.
//
// Where two plans carry identical quantities the lowest tier wins, for the same
// reason Default resolves that way: the quantities cannot distinguish them, and
// the cheaper reading is the defensible one.
func (c *Catalogue) Match(quotas *gentianov1alpha1.TenantQuotas) *gentianov1alpha1.ResourcePlan {
	if c == nil || quotas == nil {
		return nil
	}
	for i := range c.Plans {
		if gentianov1alpha1.QuotasEqual(&c.Plans[i].Spec.Quotas, quotas) {
			return &c.Plans[i]
		}
	}
	return nil
}

// Resolution is what a tenant's current plan is, and how confidently.
type Resolution struct {
	// Plan is the plan in force, or nil when the tenant's quotas match none.
	Plan *gentianov1alpha1.ResourcePlan
	// Annotated is the plan name the tenant was last set to, whether or not its
	// quotas still match. Empty when the tenant has never been set through the
	// resources API.
	Annotated string
	// Drifted is true when the annotation names a plan whose quotas no longer
	// match the tenant's — a hand edit to tenant.yaml, or a plan whose
	// quantities were changed under tenants already on it.
	//
	// Surfaced rather than resolved: the quotas are what the cluster enforces,
	// so they win, but which plan the tenant is *billed* for is a question the
	// two answers differently and a human should see that.
	Drifted bool
	// Custom is true when the tenant's quotas match no plan at all.
	Custom bool
}

// Resolve determines the plan a tenant is on.
func (c *Catalogue) Resolve(tenant *gentianov1alpha1.Tenant) Resolution {
	res := Resolution{}
	if tenant == nil {
		return res
	}
	res.Annotated = tenant.Annotations[gentianov1alpha1.ResourcePlanAnnotation]

	matched := c.Match(tenant.Spec.Quotas)
	if matched == nil {
		// No quotas at all is not a custom ceiling, it is an unset one: the
		// tenant runs unbounded, and the default plan is what it would be
		// moved onto. Saying "custom" there would invite an operator to
		// preserve a ceiling that does not exist.
		if tenant.Spec.Quotas == nil {
			res.Plan = c.Default()
			return res
		}
		res.Custom = true
		return res
	}

	res.Plan = matched
	if res.Annotated != "" && res.Annotated != matched.Name {
		res.Drifted = true
	}
	return res
}
