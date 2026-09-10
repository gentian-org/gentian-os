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
	"strconv"
	"strings"

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

// MaxTier reads a tenant's entitlement ceiling, or nil when it has none.
//
// Resolved from the Tenant rather than accepted from the caller: a ceiling
// supplied in a request is a ceiling a request can omit, and the console is not
// the only thing that can reach the resources API.
//
// An unparseable annotation is treated as absent rather than as zero. Zero is
// the base tier and would silently pin a tenant to the smallest plan on the
// cluster, which is a worse answer to a typo than no ceiling at all — and the
// cluster operator who typed it will see the tenant able to upgrade, which is
// the visible failure of the two.
func MaxTier(tenant *gentianov1alpha1.Tenant) *int32 {
	if tenant == nil {
		return nil
	}
	raw, ok := tenant.Annotations[gentianov1alpha1.MaxResourceTierAnnotation]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return nil
	}
	out := int32(parsed)
	return &out
}

// Selectable filters the catalogue to the plans a caller may move a tenant to.
//
// maxTier, when non-nil, is the tenant's entitlement ceiling: plans above it
// are withheld. A nil ceiling means none is set, and every plan is on offer —
// the same shape the App Store uses, where an unreachable commerce backend
// leaves proprietary apps on Buy rather than blocking the catalogue.
func (c *Catalogue) Selectable(selfService bool, maxTier *int32) []gentianov1alpha1.ResourcePlan {
	if c == nil {
		return nil
	}
	out := make([]gentianov1alpha1.ResourcePlan, 0, len(c.Plans))
	for i := range c.Plans {
		plan := c.Plans[i]
		if selfService && plan.Spec.SelfServiceDisabled {
			continue
		}
		if maxTier != nil && plan.Spec.Tier > *maxTier {
			continue
		}
		out = append(out, plan)
	}
	return out
}
