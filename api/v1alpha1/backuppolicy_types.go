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
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// errMissingDestinationCredentials is returned rather than silently falling
// back to the platform's own credentials, which authenticate to the platform's
// own MinIO and would fail — or worse, succeed — against a different endpoint.
var errMissingDestinationCredentials = errors.New(
	"a destination endpoint requires credentialsSecretRef: the platform's own credentials do not authenticate elsewhere")

// BackupDestination names object storage to write bundles to.
//
// The platform's own MinIO is the default and needs none of this. A
// destination is stated when bundles should live somewhere else — which, on a
// cluster whose MinIO shares a disk with the data it protects, is the only
// arrangement that survives losing that disk.
type BackupDestination struct {
	// Endpoint is the S3 API URL, e.g. https://s3.eu-central-1.amazonaws.com
	// or https://minio.example.org:9000. Empty means the platform's own.
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

	// Region, for providers that require one. Empty suits MinIO and most
	// S3-compatible stores.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef holds accessKey and secretKey for the endpoint.
	//
	// Required whenever Endpoint is set: the platform's own MinIO credentials
	// authenticate to the platform's own MinIO and nothing else, and quietly
	// reusing them against someone else's endpoint would send a tenant's data
	// to a store it cannot open. Cluster policy reads this from the kernel
	// namespace; a tenant's policy reads it from that tenant's namespace, so a
	// tenant can never name a Secret it does not own.
	// +optional
	CredentialsSecretRef *SecretKeyRef `json:"credentialsSecretRef,omitempty"`
}

// IsSet reports whether this destination asks for anything other than the
// platform's own storage.
func (d *BackupDestination) IsSet() bool {
	return d != nil && (d.Endpoint != "" || d.Bucket != "")
}

// Validate reports why a destination cannot be used.
func (d *BackupDestination) Validate() error {
	if d == nil || d.Endpoint == "" {
		return nil
	}
	if d.CredentialsSecretRef == nil || d.CredentialsSecretRef.Name == "" {
		return errMissingDestinationCredentials
	}
	return nil
}

// BackupPolicySpec is the cluster's default backup arrangement.
type BackupPolicySpec struct {
	// Destination is where bundles are written unless a tenant overrides it.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// Schedule is the default cron expression, in UTC, applied to tenants that
	// do not state their own. Empty means no scheduled backups — a decision
	// worth making deliberately rather than inheriting.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Schedule string `json:"schedule,omitempty"`

	// KeepLast is the default retention for scheduled exports. Zero keeps
	// everything, which is only right when something else prunes.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepLast int32 `json:"keepLast,omitempty"`

	// AllowTenantOverride lets a tenant point its bundles at its own storage.
	//
	// On by default. Turning it off is how an operator keeps every tenant's
	// bundles in one place they control — worth stating, because a tenant
	// writing to storage the platform cannot read is also a tenant the
	// platform cannot help restore.
	// +optional
	// +kubebuilder:default=true
	AllowTenantOverride *bool `json:"allowTenantOverride,omitempty"`
}

// OverrideAllowed reports whether tenants may override this policy.
func (s *BackupPolicySpec) OverrideAllowed() bool {
	if s == nil || s.AllowTenantOverride == nil {
		return true
	}
	return *s.AllowTenantOverride
}

// BackupPolicyStatus reports what the policy resolved to.
type BackupPolicyStatus struct {
	// Conditions carries Accepted: whether the destination is usable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// BackupPolicy is the cluster-wide default for tenant backups.
//
// Cluster-scoped and singleton by convention (`default`): a second one would
// leave "which destination applies" answerable two ways.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bkpol
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.destination.endpoint`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.destination.bucket`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
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

// TenantBackupPolicySpec overrides the cluster default for one tenant.
//
// Every field is optional and an unset field inherits. That is what makes
// "change only the schedule" expressible without restating a destination the
// tenant never chose.
type TenantBackupPolicySpec struct {
	// Destination overrides where this tenant's bundles are written.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// Schedule overrides the cluster's cron expression, in UTC. The empty
	// string inherits; use SuspendSchedule to mean "none".
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Schedule string `json:"schedule,omitempty"`

	// SuspendSchedule turns scheduled backups off for this tenant, distinctly
	// from inheriting. Without it, "no schedule" and "not stated" would be the
	// same value and a tenant could not opt out of a cluster default.
	// +optional
	SuspendSchedule bool `json:"suspendSchedule,omitempty"`

	// KeepLast overrides retention. Nil inherits; zero means keep everything.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepLast *int32 `json:"keepLast,omitempty"`
}

// TenantBackupPolicyStatus reports what this tenant's backups resolved to.
type TenantBackupPolicyStatus struct {
	// EffectiveEndpoint, EffectiveBucket and EffectiveSchedule are the values
	// actually in force after inheritance, published so an admin can see what
	// applies without recomputing the merge.
	// +optional
	EffectiveEndpoint string `json:"effectiveEndpoint,omitempty"`
	// +optional
	EffectiveBucket string `json:"effectiveBucket,omitempty"`
	// +optional
	EffectiveSchedule string `json:"effectiveSchedule,omitempty"`

	// Conditions carries Accepted: whether the override is permitted and its
	// destination usable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TenantBackupPolicy overrides the cluster backup policy for one tenant.
//
// Namespaced and singleton by convention (`default`) in the tenant's own
// namespace, which is also what keeps a tenant's credential reference inside
// the namespace it controls.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tbkpol
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.effectiveEndpoint`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.effectiveBucket`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.status.effectiveSchedule`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TenantBackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantBackupPolicySpec   `json:"spec,omitempty"`
	Status TenantBackupPolicyStatus `json:"status,omitempty"`
}

// TenantBackupPolicyList contains a list of TenantBackupPolicy.
// +kubebuilder:object:root=true
type TenantBackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantBackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&BackupPolicy{}, &BackupPolicyList{},
		&TenantBackupPolicy{}, &TenantBackupPolicyList{},
	)
}
