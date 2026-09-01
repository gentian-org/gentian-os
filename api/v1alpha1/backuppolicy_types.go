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

// BackupDestination names object storage to write bundles to.
//
// The platform's own MinIO is the default and needs none of this. A
// destination is stated when bundles should live somewhere else — which, on a
// cluster whose MinIO shares a disk with the data it protects, is the only
// arrangement that survives losing that disk.
//
// There is deliberately no credential field. The keys for an endpoint are a
// CredentialRequirement, which the operator derives from this policy and the
// credential manager fills: the requirement validates the keys before anything
// depends on them, rotation is one write at one path, and ESO's sync status is
// the satisfaction probe. A Secret reference here would have been a second,
// hand-managed way to hold the same credential with none of that.
type BackupDestination struct {
	// Endpoint is the S3 API URL, e.g. https://sos-ch-gva-2.exo.io or
	// https://minio.example.org:9000. Empty means the platform's own storage.
	//
	// A scheme is required: "s3.example.org" is ambiguous about TLS, and a
	// backup silently sent in plaintext is worse than one that fails.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^$|^https?://[^\s/]+(/.*)?$`
	Endpoint string `json:"endpoint,omitempty"`

	// Bucket holds the bundles. Empty keeps the per-tenant default name.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	Bucket string `json:"bucket,omitempty"`

	// Region, for providers that require one — Exoscale SOS and AWS do, MinIO
	// does not.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Region string `json:"region,omitempty"`
}

// IsSet reports whether this destination asks for anything other than the
// platform's own storage.
func (d *BackupDestination) IsSet() bool {
	return d != nil && (d.Endpoint != "" || d.Bucket != "")
}

// NeedsCredential reports whether this destination requires keys of its own.
// A bucket rename within the platform's own storage does not.
func (d *BackupDestination) NeedsCredential() bool {
	return d != nil && d.Endpoint != ""
}

// BackupRetention decides which bundles survive a sweep.
//
// KeepLast alone answers "how far back can I go" only in nights. The tiers
// below answer it in months without storing a bundle a night: keep every
// recent one, then one a day, one a week, one a month, one a year — density
// falling with age, which is how far back anyone actually needs to reach.
type BackupRetention struct {
	// KeepLast retains this many most recent finished exports regardless of
	// age. Zero relies entirely on the tiers below; with no tiers either,
	// nothing is deleted — a decision rather than a default, and the right one
	// only when something else prunes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepLast int32 `json:"keepLast,omitempty"`

	// KeepDaily, KeepWeekly, KeepMonthly and KeepYearly each retain the most
	// recent export within that many distinct periods. A bundle kept by any
	// tier survives; the tiers are a union, not a sequence.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepDaily int32 `json:"keepDaily,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepWeekly int32 `json:"keepWeekly,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepMonthly int32 `json:"keepMonthly,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepYearly int32 `json:"keepYearly,omitempty"`
}

// IsSet reports whether any retention rule was stated.
func (r *BackupRetention) IsSet() bool {
	if r == nil {
		return false
	}
	return r.KeepLast > 0 || r.KeepDaily > 0 || r.KeepWeekly > 0 ||
		r.KeepMonthly > 0 || r.KeepYearly > 0
}

// BackupPolicySpec is one backup arrangement: the cluster's default, or one
// tenant's override of it.
//
// One type for both, distinguished by Scope, because they carry the same
// fields and a tenant's policy is exactly the cluster's with some fields
// restated. Two types meant two schemas, two status shapes and a merge that
// could disagree with itself.
//
// +kubebuilder:validation:XValidation:rule="self.scope == 'tenant' ? (has(self.tenant) && self.tenant != '') : (!has(self.tenant) || self.tenant == '')",message="tenant is required when scope is tenant, and must be empty when scope is cluster"
type BackupPolicySpec struct {
	// Scope decides whether this is the cluster default or one tenant's
	// override, and with it who may edit the policy and which OpenBao path its
	// credential lives at. A tenant admin must not reach cluster paths.
	// +kubebuilder:validation:Enum=cluster;tenant
	Scope string `json:"scope"`

	// Tenant names the tenant a tenant-scoped policy belongs to. Empty for
	// cluster scope, required for tenant scope; the CEL rule above rejects
	// either mistake at admission, because a tenant-scoped policy naming no
	// tenant would apply to all of them or none.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Tenant string `json:"tenant,omitempty"`

	// Destination is where bundles are written. Unset inherits: the cluster
	// default for a tenant policy, the platform's own storage for the cluster.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// Schedule is a five-field cron expression, in UTC. Empty inherits; use
	// SuspendSchedule to mean none.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Schedule string `json:"schedule,omitempty"`

	// SuspendSchedule turns scheduled backups off, distinctly from inheriting.
	// Without it "no schedule" and "not stated" would be the same value, and a
	// tenant could not opt out of a cluster-wide schedule.
	// +optional
	SuspendSchedule bool `json:"suspendSchedule,omitempty"`

	// Retention decides which bundles survive. Unset inherits.
	// +optional
	Retention *BackupRetention `json:"retention,omitempty"`

	// AllowTenantOverride lets tenants state policies of their own. Read only
	// from the cluster-scoped policy; meaningless on a tenant's own.
	//
	// On by default. Turning it off keeps every tenant's bundles in storage
	// the operator controls — worth stating, because bundles the platform
	// cannot reach are bundles it cannot help restore.
	// +optional
	// +kubebuilder:default=true
	AllowTenantOverride *bool `json:"allowTenantOverride,omitempty"`
}

// OverrideAllowed reports whether tenants may state policies of their own.
func (s *BackupPolicySpec) OverrideAllowed() bool {
	if s == nil || s.AllowTenantOverride == nil {
		return true
	}
	return *s.AllowTenantOverride
}

// BackupPolicyStatus reports what this policy resolved to and whether it can
// actually be used.
type BackupPolicyStatus struct {
	// EffectiveEndpoint, EffectiveBucket and EffectiveSchedule are the values
	// in force after inheritance, published so an admin reads what applies
	// rather than recomputing the merge.
	// +optional
	EffectiveEndpoint string `json:"effectiveEndpoint,omitempty"`
	// +optional
	EffectiveBucket string `json:"effectiveBucket,omitempty"`
	// +optional
	EffectiveSchedule string `json:"effectiveSchedule,omitempty"`

	// CredentialRequirement names the requirement carrying this destination's
	// keys, and CredentialSatisfied reports whether it has been filled. A
	// destination whose credential is unsatisfied is a policy that will fail
	// at 03:00; surfaced here it is a red field in the console instead.
	// +optional
	CredentialRequirement string `json:"credentialRequirement,omitempty"`
	// +optional
	CredentialSatisfied bool `json:"credentialSatisfied,omitempty"`

	// Conditions carries Accepted: whether this policy is permitted and usable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// BackupPolicy is where a tenant's bundles go, how often, and how long they
// are kept.
//
// Cluster-scoped for both scopes, as CredentialRequirement is: the console
// mediates access by spec.tenant, and the credential itself is governed by the
// OpenBao policy its scope selects.
//
// The name is not free. Every reader of a policy fetches it by name — the
// cluster's as "default", a tenant's as the tenant's own name — so a policy
// whose name does not match its scope is never read by anything. That failed
// silently: the policy reported Accepted, published an effective destination,
// and bundles went to the platform's own storage as though it had never been
// written. The two rules below make that object unadmittable rather than
// merely ineffective.
//
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.scope != 'tenant' || self.metadata.name == self.spec.tenant",message="a tenant-scoped BackupPolicy must be named after the tenant it applies to: metadata.name must equal spec.tenant, because that is the name the operator reads it by"
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.scope != 'cluster' || self.metadata.name == 'default'",message="the cluster-scoped BackupPolicy is a singleton and must be named 'default', because that is the name the operator reads it by"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bkpol
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.effectiveEndpoint`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.effectiveBucket`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.status.effectiveSchedule`
// +kubebuilder:printcolumn:name="Credential",type=boolean,JSONPath=`.status.credentialSatisfied`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupPolicySpec   `json:"spec,omitempty"`
	Status BackupPolicyStatus `json:"status,omitempty"`
}

// BackupPolicyList contains a list of BackupPolicy.
// +kubebuilder:object:root=true
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupPolicy{}, &BackupPolicyList{})
}
