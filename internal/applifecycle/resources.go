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

package applifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/resourceplan"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// PlanSummary is one plan as the API presents it.
type PlanSummary struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description,omitempty"`
	Tier        int32             `json:"tier"`
	ProductSku  string            `json:"productSku,omitempty"`
	Quotas      map[string]string `json:"quotas"`
	// Current marks the plan the tenant is on.
	Current bool `json:"current,omitempty"`
	// Selectable is false when the plan exists but this caller may not move to
	// it. Returned rather than filtered out so a tenant admin can see the
	// upgrade that exists above their entitlement instead of wondering whether
	// the platform offers one at all.
	Selectable bool `json:"selectable"`
	// Blocked, when set, says why Selectable is false in the caller's terms.
	Blocked string `json:"blocked,omitempty"`
	// BlockedBy names the rule behind Blocked, for a caller that has to act
	// on it rather than show it: the director turns a plan that does not fit
	// into 409 and one the tenant is not entitled to into 402, and it cannot
	// tell those apart from a sentence.
	BlockedBy PlanBlock `json:"blockedBy,omitempty"`
}

// PlanBlock is why a plan is not selectable for a tenant.
type PlanBlock string

const (
	// PlanBlockedSelfService: the plan is arranged with the platform operator
	// and a tenant administrator cannot pick it.
	PlanBlockedSelfService PlanBlock = "self-service"
	// PlanBlockedEntitlement: the plan is above the tenant's entitlement.
	PlanBlockedEntitlement PlanBlock = "entitlement"
	// PlanBlockedFit: the tenant's committed usage does not fit under the plan.
	PlanBlockedFit PlanBlock = "fit"
)

// ResourceStateResult is a tenant's ceiling, consumption and plan.
type ResourceStateResult struct {
	Tenant string `json:"tenant"`
	// Plan is the plan whose quotas match the tenant's, empty when none does.
	Plan string `json:"plan,omitempty"`
	// AnnotatedPlan is the plan the tenant was last set to through this API.
	AnnotatedPlan string `json:"annotatedPlan,omitempty"`
	// Drifted reports the two disagreeing: the ceiling in force is not the one
	// the recorded plan describes, so what is enforced and what is billed have
	// come apart and someone should decide which is right.
	Drifted bool `json:"drifted,omitempty"`
	// Custom reports a ceiling matching no plan — a hand-edited tenant.yaml.
	Custom bool `json:"custom,omitempty"`
	// Quota is the enforced ceiling paired with current consumption.
	Quota []resourceplan.Headroom `json:"quota"`
	// HasQuota is false for a tenant running with no ceiling at all, which an
	// empty Quota list would otherwise be indistinguishable from.
	HasQuota bool `json:"hasQuota"`
	// Actual is live consumption, absent when no source is configured.
	Actual map[string]string `json:"actual,omitempty"`
	// ActualSource names where Actual came from, or why it is missing.
	ActualSource string `json:"actualSource,omitempty"`
	// InstalledApps counts entries in spec.apps. Shown for context, not
	// checked against the plan: an app cap is the Tenant webhook's policy
	// limit, not part of the capacity a plan sells.
	InstalledApps int `json:"installedApps"`
}

// ResourceState reports a tenant's current ceiling, consumption and plan.
func (s *Service) ResourceState(ctx context.Context, tenantName string) (*ResourceStateResult, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	catalogue, err := resourceplan.Load(ctx, s.client)
	if err != nil {
		return nil, err
	}
	resolution := catalogue.Resolve(tenant)

	result := &ResourceStateResult{
		Tenant:        tenant.Name,
		AnnotatedPlan: resolution.Annotated,
		Drifted:       resolution.Drifted,
		Custom:        resolution.Custom,
		InstalledApps: len(tenant.Spec.Apps),
	}
	if resolution.Plan != nil {
		result.Plan = resolution.Plan.Name
	}

	quota, err := s.tenantQuota(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if quota != nil {
		result.HasQuota = true
		result.Quota = resourceplan.Describe(quota)
	}

	if s.actualSource == nil {
		result.ActualSource = "unavailable: no metrics source is configured for this cluster"
	} else if actual, err := s.actualSource.NamespaceUsage(ctx, tenant.NamespaceName()); err != nil {
		// Named rather than swallowed. A chart with a missing series and no
		// explanation reads as "this tenant used nothing", which is the
		// opposite of what an unreachable metrics API means.
		result.ActualSource = fmt.Sprintf("unavailable: %s did not answer", s.actualSource.Name())
	} else {
		result.ActualSource = s.actualSource.Name()
		result.Actual = map[string]string{}
		for name, q := range actual {
			result.Actual[string(name)] = q.String()
		}
	}
	return result, nil
}

// Plans lists the catalogue as it applies to one tenant.
func (s *Service) Plans(
	ctx context.Context,
	tenantName string,
	selfService bool,
) ([]PlanSummary, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	catalogue, err := resourceplan.Load(ctx, s.client)
	if err != nil {
		return nil, err
	}
	resolution := catalogue.Resolve(tenant)
	currentName := ""
	if resolution.Plan != nil {
		currentName = resolution.Plan.Name
	}
	maxTier := resourceplan.MaxTier(tenant)

	quota, err := s.tenantQuota(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var used corev1.ResourceList
	if quota != nil {
		used = quota.Status.Used
	}

	out := make([]PlanSummary, 0, len(catalogue.Plans))
	for i := range catalogue.Plans {
		plan := &catalogue.Plans[i]
		summary := PlanSummary{
			Name:        plan.Name,
			DisplayName: plan.Spec.DisplayName,
			Description: plan.Spec.Description,
			Tier:        plan.Spec.Tier,
			ProductSku:  plan.Spec.ProductSku,
			Quotas:      quotaMap(&plan.Spec.Quotas),
			Current:     plan.Name == currentName,
			Selectable:  true,
		}
		switch {
		case selfService && plan.Spec.SelfServiceDisabled:
			summary.Selectable = false
			summary.Blocked = "this plan is arranged with the platform operator, not self-service"
			summary.BlockedBy = PlanBlockedSelfService
		case maxTier != nil && plan.Spec.Tier > *maxTier:
			summary.Selectable = false
			summary.Blocked = "this plan is above the tenant's current entitlement"
			summary.BlockedBy = PlanBlockedEntitlement
		default:
			if err := resourceplan.CheckFit(plan, used); err != nil {
				var downgrade *resourceplan.DowngradeError
				if errors.As(err, &downgrade) {
					summary.Selectable = false
					summary.Blocked = downgrade.Error()
					summary.BlockedBy = PlanBlockedFit
				}
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// UsageHistory returns thinned samples over a window.
func (s *Service) UsageHistory(
	ctx context.Context,
	tenantName string,
	from, to time.Time,
	step time.Duration,
) ([]usage.Sample, error) {
	store, err := usage.StoreForTenant(ctx, s.client, layout.Tenant(tenantName), tenantName)
	if err != nil {
		return nil, err
	}
	return store.Samples(ctx, from, to, step)
}

// UsageReport resolves a window into billable plan intervals.
func (s *Service) UsageReport(
	ctx context.Context,
	tenantName string,
	from, to time.Time,
) (*usage.Report, error) {
	store, err := usage.StoreForTenant(ctx, s.client, layout.Tenant(tenantName), tenantName)
	if err != nil {
		return nil, err
	}
	return usage.BuildReport(ctx, store, tenantName, from, to)
}

func (s *Service) getTenant(ctx context.Context, name string) (*gentianov1alpha1.Tenant, error) {
	var tenant gentianov1alpha1.Tenant
	if err := s.client.Get(ctx, types.NamespacedName{Name: name}, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("tenant %q not found", name)
		}
		return nil, err
	}
	return &tenant, nil
}

func (s *Service) tenantQuota(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
) (*corev1.ResourceQuota, error) {
	var quota corev1.ResourceQuota
	key := types.NamespacedName{Name: usage.TenantQuotaName, Namespace: tenant.NamespaceName()}
	if err := s.client.Get(ctx, key, &quota); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &quota, nil
}

func quotaMap(q *gentianov1alpha1.TenantQuotas) map[string]string {
	out := map[string]string{}
	if q == nil {
		return out
	}
	if q.RequestsCPU != nil {
		out["requestsCpu"] = q.RequestsCPU.String()
	}
	if q.RequestsMemory != nil {
		out["requestsMemory"] = q.RequestsMemory.String()
	}
	if q.CPU != nil {
		out["cpu"] = q.CPU.String()
	}
	if q.Memory != nil {
		out["memory"] = q.Memory.String()
	}
	if q.Storage != nil {
		out["storage"] = q.Storage.String()
	}
	if q.MaxPods > 0 {
		out["maxPods"] = fmt.Sprintf("%d", q.MaxPods)
	}
	return out
}
