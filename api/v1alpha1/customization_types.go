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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Customization records one deliberate deviation from an app as shipped.
//
// A record is required for every customization at rung L2 and above, and is
// written before the code — it is the design review. The schema is modelled on
// Debian DEP-3 patch headers, generalised to the whole ladder: it captures not
// just what was changed but why nothing cheaper would do, whether upstream was
// offered the change, and when the deviation should be reconsidered.
//
// See docs/app-customization.md §5.

// CustomizationScope is the blast radius of a customization. It is independent
// of the rung: both are minimised separately.
// +kubebuilder:validation:Enum=tenant;profile;platform
type CustomizationScope string

const (
	// ScopeTenant affects one tenant's install.
	ScopeTenant CustomizationScope = "tenant"
	// ScopeProfile affects every tenant that installs the profile.
	ScopeProfile CustomizationScope = "profile"
	// ScopePlatform affects every tenant and every app. Only valid for mechanisms
	// that are generic across apps.
	ScopePlatform CustomizationScope = "platform"
)

// CustomizationAuthorship records who wrote a customization. The ladder is
// authorship-neutral — the rung depends on what the change does, never on who
// wrote it — but the record carries authorship so ownership can be delegated
// later as a policy change rather than a data migration.
// +kubebuilder:validation:Enum=gentian;tenant;supplier;partner;community
type CustomizationAuthorship string

// CustomizationRepoOwnership records who controls the backing repository.
// +kubebuilder:validation:Enum=gentian;external
type CustomizationRepoOwnership string

// CustomizationSupportContract records the support terms attached to a
// customization authored outside Gentian.
// +kubebuilder:validation:Enum=none;community;commercial
type CustomizationSupportContract string

// CustomizationUpgradeRisk estimates what an upstream version bump will cost.
// +kubebuilder:validation:Enum=low;medium;high
type CustomizationUpgradeRisk string

// CustomizationForwarded mirrors the DEP-3 Forwarded field: whether the change
// was offered upstream. "no" requires a written reason.
// +kubebuilder:validation:Enum=yes;no;not-needed
type CustomizationForwarded string

// CustomizationLicensingEffect records whether a customization changes the
// licensing position of the app it modifies.
// +kubebuilder:validation:Enum=none;adds-dependency;changes-terms
type CustomizationLicensingEffect string

// CustomizationTarget identifies the app a customization applies to.
type CustomizationTarget struct {
	// Family is the logical app id shared across profile revisions.
	// +optional
	Family string `json:"family,omitempty"`

	// Profile is the AppProfile name this customization targets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Profile string `json:"profile"`

	// AppVersion is the upstream application version this was built against.
	// +optional
	AppVersion string `json:"appVersion,omitempty"`

	// ChartVersion is the chart version this was built against.
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
}

// CustomizationUpstreamFirst records the upstream-first obligation. Carrying a
// downstream delta is a debt with a due date, not a decision — this block is
// what makes that reviewable.
type CustomizationUpstreamFirst struct {
	// Attempted reports whether upstream was searched and, where applicable,
	// offered the change. Required to be true for rung L4 and above.
	// +optional
	Attempted bool `json:"attempted,omitempty"`

	// Forwarded mirrors DEP-3: was the change sent upstream?
	// +optional
	Forwarded CustomizationForwarded `json:"forwarded,omitempty"`

	// URL links the upstream issue or pull request.
	// +optional
	URL string `json:"url,omitempty"`

	// AppliedUpstream reports whether upstream has accepted the change. When true
	// the customization should be scheduled for removal at the next version bump.
	// +optional
	AppliedUpstream bool `json:"appliedUpstream,omitempty"`

	// Reason explains a Forwarded value of "no" or "not-needed".
	// +optional
	Reason string `json:"reason,omitempty"`
}

// CustomizationArtifact points at the code or content implementing the change.
type CustomizationArtifact struct {
	// Repo is the repository holding the artifact.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Path is the repo-relative location of the artifact.
	// +optional
	Path string `json:"path,omitempty"`

	// Version pins the artifact.
	// +optional
	Version string `json:"version,omitempty"`
}

// CustomizationOrigin records authorship and repository ownership.
type CustomizationOrigin struct {
	// Authorship records who wrote the customization.
	// +optional
	// +kubebuilder:default=gentian
	Authorship CustomizationAuthorship `json:"authorship,omitempty"`

	// Organisation names the authoring organisation, when not Gentian.
	// +optional
	Organisation string `json:"organisation,omitempty"`

	// Contact is the maintainer contact for the customization.
	// +optional
	Contact string `json:"contact,omitempty"`

	// Repo is the backing repository, when externally owned.
	// +optional
	Repo string `json:"repo,omitempty"`

	// RepoOwnership records who controls that repository.
	// +optional
	// +kubebuilder:default=gentian
	RepoOwnership CustomizationRepoOwnership `json:"repoOwnership,omitempty"`

	// ReviewedBy names the role at Gentian that accepted the customization.
	// +optional
	ReviewedBy string `json:"reviewedBy,omitempty"`

	// SupportContract records the support terms.
	// +optional
	// +kubebuilder:default=none
	SupportContract CustomizationSupportContract `json:"supportContract,omitempty"`
}

// CustomizationSecurity records the security posture of a customization.
type CustomizationSecurity struct {
	// MACWaivers lists waivers this customization requires. Requests are still
	// approved through PlatformSecurityPolicy — declaring one here does not grant it.
	// +optional
	// +listType=set
	MACWaivers []string `json:"macWaivers,omitempty"`

	// NewEgress lists network destinations the customization introduces.
	// +optional
	// +listType=set
	NewEgress []string `json:"newEgress,omitempty"`

	// HandlesPersonalData flags customizations that process personal data.
	// +optional
	HandlesPersonalData bool `json:"handlesPersonalData,omitempty"`
}

// CustomizationLicensing records the licensing effect of a customization.
type CustomizationLicensing struct {
	// Effect records whether licensing terms change.
	// +optional
	// +kubebuilder:default=none
	Effect CustomizationLicensingEffect `json:"effect,omitempty"`

	// Notes explains a non-none effect.
	// +optional
	Notes string `json:"notes,omitempty"`
}

// CustomizationSpec is the desired state of a Customization record.
type CustomizationSpec struct {
	// Summary states what the customization does, in one line.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Summary string `json:"summary"`

	// Target identifies the app being customized.
	// +kubebuilder:validation:Required
	Target CustomizationTarget `json:"target"`

	// Rung is the ladder rung this customization sits at.
	// +kubebuilder:validation:Required
	Rung CustomizationRung `json:"rung"`

	// Scope is the blast radius.
	// +kubebuilder:validation:Required
	Scope CustomizationScope `json:"scope"`

	// Tenants lists affected tenants. Required and non-empty when Scope is tenant.
	// +optional
	// +listType=set
	Tenants []string `json:"tenants,omitempty"`

	// RungJustification explains, per rung skipped, why nothing cheaper would do.
	// Keys are rung identifiers ("L0", "L1", ...). This is the field that stops the
	// ladder from being decorative.
	// +optional
	RungJustification map[string]string `json:"rungJustification,omitempty"`

	// UpstreamFirst records the upstream-first obligation.
	// +optional
	UpstreamFirst *CustomizationUpstreamFirst `json:"upstreamFirst,omitempty"`

	// Artifacts point at the implementing code or content.
	// +optional
	Artifacts []CustomizationArtifact `json:"artifacts,omitempty"`

	// Delivery is how an L3 module reaches the running app.
	// +optional
	Delivery CustomizationDelivery `json:"delivery,omitempty"`

	// Origin records authorship and repository ownership.
	// +optional
	Origin *CustomizationOrigin `json:"origin,omitempty"`

	// Owner is the team or person accountable. Free-form rather than a Gentian
	// team enum, so an external maintainer can hold it without a schema change.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// Created is the date the record was written (RFC 3339 date).
	// +optional
	// Accepts a bare date and, defensively, the midnight-UTC RFC3339 form YAML
	// parsers commonly normalize an unquoted date scalar into (e.g.
	// sigs.k8s.io/yaml turns 2027-08-06 into 2027-08-06T00:00:00Z on its way to
	// JSON) — authors should still quote date values in YAML, but a record
	// should not fail admission over a YAML parser quirk.
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}(T00:00:00Z)?$`
	Created string `json:"created,omitempty"`

	// ReviewBy is the date this deviation must be reconsidered. A record whose
	// review date has passed is customization debt, and is reported as such.
	// +kubebuilder:validation:Required
	// Accepts a bare date and, defensively, the midnight-UTC RFC3339 form YAML
	// parsers commonly normalize an unquoted date scalar into (e.g.
	// sigs.k8s.io/yaml turns 2027-08-06 into 2027-08-06T00:00:00Z on its way to
	// JSON) — authors should still quote date values in YAML, but a record
	// should not fail admission over a YAML parser quirk.
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}(T00:00:00Z)?$`
	ReviewBy string `json:"reviewBy"`

	// ExitCriteria states what would allow this customization to be dropped.
	// +optional
	ExitCriteria string `json:"exitCriteria,omitempty"`

	// UpgradeRisk estimates the cost of the next upstream version bump.
	// +optional
	// +kubebuilder:default=medium
	UpgradeRisk CustomizationUpgradeRisk `json:"upgradeRisk,omitempty"`

	// TestedAgainst lists the app versions this customization was verified against.
	// +optional
	// +listType=set
	TestedAgainst []string `json:"testedAgainst,omitempty"`

	// Tests lists the regression tests covering this customization.
	// +optional
	// +listType=set
	Tests []string `json:"tests,omitempty"`

	// Security records the security posture.
	// +optional
	Security *CustomizationSecurity `json:"security,omitempty"`

	// Licensing records the licensing effect.
	// +optional
	Licensing *CustomizationLicensing `json:"licensing,omitempty"`
}

// CustomizationPhase summarises the health of a customization record.
// +kubebuilder:validation:Enum=Active;NeedsReview;Superseded;Invalid
type CustomizationPhase string

const (
	// CustomizationPhaseActive means the record is valid and within its review window.
	CustomizationPhaseActive CustomizationPhase = "Active"
	// CustomizationPhaseNeedsReview means the record is past its review date or has drifted.
	CustomizationPhaseNeedsReview CustomizationPhase = "NeedsReview"
	// CustomizationPhaseSuperseded means upstream has taken the change.
	CustomizationPhaseSuperseded CustomizationPhase = "Superseded"
	// CustomizationPhaseInvalid means the record does not resolve to a real target.
	CustomizationPhaseInvalid CustomizationPhase = "Invalid"
)

// CustomizationStatus is the observed state, computed by the operator. It exists
// so the debt report is derived from live cluster state rather than guessed from
// YAML by a script.
type CustomizationStatus struct {
	// Phase summarises record health.
	// +optional
	Phase CustomizationPhase `json:"phase,omitempty"`

	// Conditions provide detailed status.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled spec generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReviewOverdue is true once spec.reviewBy has passed.
	// +optional
	ReviewOverdue bool `json:"reviewOverdue,omitempty"`

	// UpstreamStale is true when the change was never forwarded upstream and no
	// reason was recorded, or when upstream has accepted it and the local delta
	// is still carried.
	// +optional
	UpstreamStale bool `json:"upstreamStale,omitempty"`

	// TargetVersionDrift is true when the target profile now pins a chart or app
	// version outside spec.testedAgainst.
	// +optional
	TargetVersionDrift bool `json:"targetVersionDrift,omitempty"`

	// RungAboveRecommended is true when the target's customization surface offers
	// a lower rung this record's rungJustification has no entry for — a rung the
	// app has started supporting since the record was written, and therefore a
	// candidate for descending. A cheaper rung the record already justified
	// skipping does not set this; every rung below spec.rung must be justified
	// to pass admission, so re-flagging those would mark every compliant record
	// permanently, defeating the signal.
	// +optional
	RungAboveRecommended bool `json:"rungAboveRecommended,omitempty"`

	// ResolvedNamespace is the namespace the target app was found in. Used by the
	// L3 per-tenant namespace test.
	// +optional
	ResolvedNamespace string `json:"resolvedNamespace,omitempty"`
}

// Customization is a tracked, owned, dated deviation from an app as shipped.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cust;customizations
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.target.profile`
// +kubebuilder:printcolumn:name="Rung",type=string,JSONPath=`.spec.rung`
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Review By",type=string,JSONPath=`.spec.reviewBy`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Customization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CustomizationSpec   `json:"spec,omitempty"`
	Status CustomizationStatus `json:"status,omitempty"`
}

// CustomizationList contains a list of Customization.
// +kubebuilder:object:root=true
type CustomizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Customization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Customization{}, &CustomizationList{})
}
