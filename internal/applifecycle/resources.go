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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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
}

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
	// InstalledApps counts entries in spec.apps, which maxApps limits.
	InstalledApps int `json:"installedApps"`
}

// SetPlanRequest moves a tenant to a plan.
type SetPlanRequest struct {
	Tenant string
	Plan   string
	Actor  string
	// SelfService marks a request made by a tenant administrator rather than a
	// cluster operator. It withholds plans flagged SelfServiceDisabled; it does
	// not relax any check, because the downgrade guard protects the tenant's
	// own workloads and a cluster operator has no more business breaking them.
	SelfService bool
	// Force skips the downgrade guard. Reserved for a cluster operator who has
	// decided the tenant will be shrunk regardless — the pods that cannot be
	// recreated afterwards are then a known cost rather than a surprise.
	Force bool
}

// SetPlanResult reports the outcome of a plan change.
type SetPlanResult struct {
	Status string `json:"status"`
	Tenant string `json:"tenant"`
	Plan   string `json:"plan"`
	// PreviousPlan is what the tenant was on, empty when it was on none.
	PreviousPlan string `json:"previousPlan,omitempty"`
	File         string `json:"file,omitempty"`
	Message      string `json:"message,omitempty"`
}

// ErrPlanNotFound is returned for a plan name absent from the catalogue.
var ErrPlanNotFound = errors.New("resource plan not found")

// ErrPlanNotSelectable is returned when a plan exists but the caller may not
// move this tenant to it.
var ErrPlanNotSelectable = errors.New("resource plan is not selectable for this tenant")

// tenantPlanLocks serializes plan changes per tenant. Two concurrent changes
// would each read the tenant's current plan before either had written, and the
// loser's plan event would record a transition that never happened.
var tenantPlanLocks sync.Map

func lockTenantPlan(tenant string) func() {
	v, _ := tenantPlanLocks.LoadOrStore(tenant, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
		case maxTier != nil && plan.Spec.Tier > *maxTier:
			summary.Selectable = false
			summary.Blocked = "this plan is above the tenant's current entitlement"
		default:
			if err := resourceplan.CheckFit(plan, used, len(tenant.Spec.Apps)); err != nil {
				var downgrade *resourceplan.DowngradeError
				if errors.As(err, &downgrade) {
					summary.Selectable = false
					summary.Blocked = downgrade.Error()
				}
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// SetPlan moves a tenant to a plan by committing the change to GitOps.
//
// The write is the same mechanism an app install uses — an edit to the
// deployments repository, pushed, and left for Argo to sync — so a tenant's
// ceiling has exactly one source of truth and a plan chosen in the console
// cannot be reverted by the next sync.
func (s *Service) SetPlan(ctx context.Context, req SetPlanRequest) (*SetPlanResult, error) {
	defer lockTenantPlan(req.Tenant)()

	tenant, err := s.getTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	catalogue, err := resourceplan.Load(ctx, s.client)
	if err != nil {
		return nil, err
	}
	plan := catalogue.Get(req.Plan)
	if plan == nil {
		return nil, fmt.Errorf("%w: %s", ErrPlanNotFound, req.Plan)
	}
	if req.SelfService && plan.Spec.SelfServiceDisabled {
		return nil, fmt.Errorf("%w: %s is arranged with the platform operator", ErrPlanNotSelectable, plan.Name)
	}
	if maxTier := resourceplan.MaxTier(tenant); maxTier != nil && plan.Spec.Tier > *maxTier {
		return nil, fmt.Errorf("%w: %s is above the tenant's entitlement", ErrPlanNotSelectable, plan.Name)
	}

	resolution := catalogue.Resolve(tenant)
	previous := ""
	if resolution.Plan != nil {
		previous = resolution.Plan.Name
	}

	if !req.Force {
		quota, err := s.tenantQuota(ctx, tenant)
		if err != nil {
			return nil, err
		}
		var used corev1.ResourceList
		if quota != nil {
			used = quota.Status.Used
		}
		if err := resourceplan.CheckFit(plan, used, len(tenant.Spec.Apps)); err != nil {
			return nil, err
		}
	}

	status, file, changed, err := s.git.SetResourcePlan(req.Tenant, plan, req.Actor)
	if err != nil {
		return nil, err
	}

	result := &SetPlanResult{
		Status:       status,
		Tenant:       req.Tenant,
		Plan:         plan.Name,
		PreviousPlan: previous,
		File:         file,
	}
	if !changed {
		result.Message = "the tenant is already on this plan"
		return result, nil
	}
	result.Message = "committed to the deployments repository; Argo CD applies it on the next sync"

	// The plan event is recorded now rather than when the sync lands, because
	// now is when the decision was made and by whom. A sync that fails leaves
	// an event describing an intent the samples will contradict, which is
	// visible; waiting for the sync would instead lose the actor, which is not
	// recoverable from anywhere else.
	s.recordPlanEvent(ctx, req.Tenant, previous, plan, req.Actor)
	return result, nil
}

// UsageHistory returns thinned samples over a window.
func (s *Service) UsageHistory(
	ctx context.Context,
	tenantName string,
	from, to time.Time,
	step time.Duration,
) ([]usage.Sample, error) {
	store, err := usage.StoreForTenant(ctx, s.client, s.opts.KernelNamespace, tenantName)
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
	store, err := usage.StoreForTenant(ctx, s.client, s.opts.KernelNamespace, tenantName)
	if err != nil {
		return nil, err
	}
	return usage.BuildReport(ctx, store, tenantName, from, to)
}

func (s *Service) recordPlanEvent(
	ctx context.Context,
	tenantName, previous string,
	plan *gentianov1alpha1.ResourcePlan,
	actor string,
) {
	store, err := usage.StoreForTenant(ctx, s.client, s.opts.KernelNamespace, tenantName)
	if err != nil {
		log.FromContext(ctx).WithName("resources").Error(err,
			"could not open the usage store to record a plan change", "tenant", tenantName)
		return
	}
	if err := store.EnsureSchema(ctx); err != nil {
		log.FromContext(ctx).WithName("resources").Error(err,
			"could not prepare the usage schema", "tenant", tenantName)
		return
	}
	err = store.RecordPlanEvent(ctx, usage.PlanEvent{
		OccurredAt: time.Now().UTC(),
		FromPlan:   previous,
		ToPlan:     plan.Name,
		ProductSku: plan.Spec.ProductSku,
		Actor:      actor,
	})
	if err != nil {
		// Not fatal to the plan change: the ceiling is already committed to
		// git and will be enforced. What is lost is the billing record's
		// precision, which the samples partially reconstruct — so the change
		// stands and the gap is logged rather than the change being refused
		// after it has already been pushed.
		log.FromContext(ctx).WithName("resources").Error(err,
			"plan change committed but not recorded in the usage history", "tenant", tenantName)
	}
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
	if q.CPU != nil {
		out["cpu"] = q.CPU.String()
	}
	if q.Memory != nil {
		out["memory"] = q.Memory.String()
	}
	if q.Storage != nil {
		out["storage"] = q.Storage.String()
	}
	if q.MaxApps > 0 {
		out["maxApps"] = fmt.Sprintf("%d", q.MaxApps)
	}
	if q.MaxPods > 0 {
		out["maxPods"] = fmt.Sprintf("%d", q.MaxPods)
	}
	return out
}
