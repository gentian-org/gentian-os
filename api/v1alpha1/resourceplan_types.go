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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourcePlan is a named preset of tenant resource limits.
//
// It is deliberately **not deployable**. Nothing reconciles a ResourcePlan and
// it owns no namespace, workload or quota of its own: it exists so that a
// tenant's ceiling is chosen from a catalogue rather than typed. Selecting one
// writes its quantities into that tenant's `spec.quotas` — see
// docs/design/resource-plans.md.
//
// Why a catalogue at all, when Tenant.spec.quotas already accepts any quantity:
// a quota that can be any number can be any number that was never sold. The
// plan is the unit the platform prices, meters and invoices, so the write path
// accepts a plan name and nothing else, and every tenant's ceiling is at all
// times one of a known, priced set. Free-form quantities remain available to
// whoever edits the deployments repository by hand, which is the cluster
// operator, which is the person entitled to make an unpriced choice.
//
// This mirrors the role AppPackage plays for addons: a curated preset that
// replaces a combinatorial choice with a named one, without becoming an
// artifact of its own.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rplan;rplans
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.spec.quotas.requestsCpu`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.quotas.requestsMemory`
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.spec.quotas.storage`
// +kubebuilder:printcolumn:name="SKU",type=string,JSONPath=`.spec.productSku`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ResourcePlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePlanSpec   `json:"spec,omitempty"`
	Status ResourcePlanStatus `json:"status,omitempty"`
}

// ResourcePlanSpec is the desired state of a ResourcePlan.
type ResourcePlanSpec struct {
	// DisplayName is the name shown in the Admin Console (e.g. "Base + 8").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description explains what the plan is for, in one or two sentences.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`

	// Tier orders plans from smallest to largest. It is the only ordering the
	// platform uses: quantities can move in different directions between two
	// plans (more CPU, less storage), so "bigger" cannot be derived from them,
	// and an entitlement ceiling has to compare something total.
	//
	// Tiers need not be contiguous; leaving gaps allows a plan to be inserted
	// later without renumbering the ones already sold.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	Tier int32 `json:"tier"`

	// Quotas are the limits a tenant on this plan receives. They are written
	// verbatim into Tenant.spec.quotas, so every field the Tenant CRD accepts
	// is settable here and nothing else is.
	// +kubebuilder:validation:Required
	Quotas TenantQuotas `json:"quotas"`

	// ProductSku identifies this plan to the commerce backend. It appears in
	// the usage report as the SKU in effect over an interval, which is what
	// makes a month resolve to something invoiceable.
	//
	// Empty means the plan is not billed — the free tier, or a cluster running
	// without commerce at all.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ProductSku string `json:"productSku,omitempty"`

	// Default marks the plan a tenant is assumed to be on when its quotas match
	// no plan in the catalogue and none has been selected. At most one plan
	// should set it; when several do, the lowest tier wins, because assuming a
	// tenant is on a bigger plan than they chose is the expensive mistake.
	// +optional
	Default bool `json:"default,omitempty"`

	// SelfServiceDisabled withholds the plan from tenant administrators. The
	// plan stays selectable by cluster administrators and by the CLI, so it
	// serves the case of a negotiated or bespoke ceiling that should not appear
	// in a tenant's own list of upgrades.
	// +optional
	SelfServiceDisabled bool `json:"selfServiceDisabled,omitempty"`
}

// ResourcePlanStatus reports observed plan state.
type ResourcePlanStatus struct {
	// TenantCount is the number of tenants whose quotas currently match this
	// plan. Maintained for the console's catalogue view; it is derived, and
	// nothing reads it to make a decision.
	// +optional
	TenantCount int32 `json:"tenantCount,omitempty"`

	// ObservedAt is when TenantCount was last recomputed.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ResourcePlanList contains a list of ResourcePlan.
// +kubebuilder:object:root=true
type ResourcePlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourcePlan `json:"items"`
}

const (
	// MaxResourceTierAnnotation caps the plans a tenant may be moved onto.
	//
	// On the Tenant rather than passed by the caller, because a ceiling a
	// request supplies is a ceiling a request can omit. The API resolves it
	// from cluster state on every call, so a client that knows nothing about
	// entitlements cannot buy an upgrade by leaving a field out.
	//
	// Set by the cluster operator, or by whatever commerce integration a
	// deployment runs — the platform reads it and does not care which. Absent
	// means uncapped, matching how an unreachable commerce backend leaves the
	// App Store's catalogue usable rather than blocking it.
	MaxResourceTierAnnotation = "gentianos.io/max-resource-tier"

	// ResourcePlanAnnotation records the plan a tenant was last set to.
	//
	// The quotas are the truth — they are what the cluster enforces — but they
	// cannot always name their plan: two plans may carry identical quantities,
	// and a hand-edited tenant.yaml may match none. The annotation says what
	// was chosen, matching says what is in force, and the two disagreeing is
	// itself worth showing rather than resolving silently.
	ResourcePlanAnnotation = "gentianos.io/resource-plan"
)

// QuotasEqual reports whether two quota sets impose the same limits.
//
// Compared by resolved quantity rather than by string, so 1Gi and 1024Mi are
// one plan and not two. A nil field means "no ceiling for this resource", which
// is distinct from a zero one and compares only with another nil.
func QuotasEqual(a, b *TenantQuotas) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return quantityEqual(a.Storage, b.Storage) &&
		quantityEqual(a.CPU, b.CPU) &&
		quantityEqual(a.Memory, b.Memory) &&
		quantityEqual(a.RequestsCPU, b.RequestsCPU) &&
		quantityEqual(a.RequestsMemory, b.RequestsMemory) &&
		a.MaxApps == b.MaxApps &&
		a.MaxPods == b.MaxPods
}

func quantityEqual(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(*b) == 0
}

func init() {
	SchemeBuilder.Register(&ResourcePlan{}, &ResourcePlanList{})
}
