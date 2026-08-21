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

package resourceplan

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/tenantshell"
)

// Shortfall is one resource a candidate plan does not have room for.
type Shortfall struct {
	// Resource is the ResourceQuota key, e.g. limits.cpu — the name the
	// cluster will use when it refuses a pod, so the message an admin reads
	// here matches the one they will find in the events.
	Resource string
	// Used is what the tenant has committed today.
	Used string
	// Offered is the ceiling the candidate plan would impose.
	Offered string
}

func (s Shortfall) String() string {
	return fmt.Sprintf("%s: using %s, plan allows %s", s.Resource, s.Used, s.Offered)
}

// DowngradeError reports a plan change that would leave the tenant over its new
// ceiling.
type DowngradeError struct {
	Plan       string
	Shortfalls []Shortfall
}

func (e *DowngradeError) Error() string {
	parts := make([]string, 0, len(e.Shortfalls))
	for _, s := range e.Shortfalls {
		parts = append(parts, s.String())
	}
	return fmt.Sprintf("plan %s is smaller than what the tenant is using (%s)",
		e.Plan, strings.Join(parts, "; "))
}

// CheckFit reports whether a tenant can move to a plan without ending up over
// its new ceiling.
//
// Kubernetes does not evict pods to fit a shrunken ResourceQuota — it refuses
// the *next* create. So a downgrade below current use is not rejected by the
// cluster and does not fail loudly: everything keeps running until something
// restarts, and then it silently does not come back. That is the failure mode
// commit 4ea0abba was written to explain after fifteen hours of it. Refusing
// the change here, naming the resource and both numbers, is the only point in
// the sequence where the person making the decision is still present.
//
// used is the tenant-quota ResourceQuota's status.used; it may be nil when the
// tenant has no quota yet, in which case nothing is over any ceiling.
//
// Only capacity is checked. A plan does not set an app cap — that is the Tenant
// webhook's policy limit, not a quantity anyone buys — so moving between plans
// cannot put a tenant over it.
func CheckFit(plan *gentianov1alpha1.ResourcePlan, used corev1.ResourceList) error {
	if plan == nil {
		return nil
	}
	offered := tenantshell.ResourceListFromQuotas(&plan.Spec.Quotas)

	var shortfalls []Shortfall
	for name, offer := range offered {
		have, ok := used[name]
		if !ok {
			continue
		}
		if have.Cmp(offer) > 0 {
			shortfalls = append(shortfalls, Shortfall{
				Resource: string(name),
				Used:     have.String(),
				Offered:  offer.String(),
			})
		}
	}

	if len(shortfalls) == 0 {
		return nil
	}
	sort.Slice(shortfalls, func(i, j int) bool {
		return shortfalls[i].Resource < shortfalls[j].Resource
	})
	return &DowngradeError{Plan: plan.Name, Shortfalls: shortfalls}
}

// Headroom is what a tenant has left under one resource of its ceiling.
type Headroom struct {
	Resource string `json:"resource"`
	Used     string `json:"used"`
	Hard     string `json:"hard"`
	// UsedRatio is used/hard in [0,1], rounded to three decimals, or nil when
	// the resource has no ceiling. A ratio needs a denominator, and reporting
	// 0 for "unlimited" would draw a bar that says the opposite of the truth.
	UsedRatio *float64 `json:"usedRatio,omitempty"`
}

// Describe pairs a quota's used and hard values per resource, for display.
func Describe(quota *corev1.ResourceQuota) []Headroom {
	if quota == nil {
		return nil
	}
	names := map[corev1.ResourceName]struct{}{}
	for name := range quota.Status.Hard {
		names[name] = struct{}{}
	}
	for name := range quota.Status.Used {
		names[name] = struct{}{}
	}

	out := make([]Headroom, 0, len(names))
	for name := range names {
		row := Headroom{Resource: string(name)}
		if used, ok := quota.Status.Used[name]; ok {
			row.Used = used.String()
		}
		hard, hasHard := quota.Status.Hard[name]
		if hasHard {
			row.Hard = hard.String()
			row.UsedRatio = ratio(quota.Status.Used[name], hard)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

func ratio(used, hard resource.Quantity) *float64 {
	denom := hard.AsApproximateFloat64()
	if denom <= 0 {
		return nil
	}
	v := used.AsApproximateFloat64() / denom
	rounded := float64(int64(v*1000+0.5)) / 1000
	return &rounded
}
