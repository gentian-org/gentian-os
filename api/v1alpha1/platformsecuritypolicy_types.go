// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlatformSecurityPolicy is the cluster singleton where platform administrators
// approve MAC waivers and other cluster-wide security posture settings.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=psp;platformsec
// +kubebuilder:printcolumn:name="Waivers",type=integer,JSONPath=`.status.allowedMacWaiverCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PlatformSecurityPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformSecurityPolicySpec   `json:"spec,omitempty"`
	Status PlatformSecurityPolicyStatus `json:"status,omitempty"`
}

// PlatformSecurityPolicySpec holds cluster-admin security configuration.
type PlatformSecurityPolicySpec struct {
	// AllowedMacWaivers lists profile/policy/scope tuples the cluster permits.
	// AppProfile requests outside this list are denied at deploy time.
	// +optional
	AllowedMacWaivers []AllowedMacWaiver `json:"allowedMacWaivers,omitempty"`
}

// PlatformSecurityPolicyStatus reports observed policy state.
type PlatformSecurityPolicyStatus struct {
	// AllowedMacWaiverCount is len(spec.allowedMacWaivers) last written to the
	// gentian-platform-security ConfigMap.
	// +optional
	AllowedMacWaiverCount int `json:"allowedMacWaiverCount,omitempty"`

	// Conditions provide reconciliation detail.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PlatformSecurityPolicyList contains a list of PlatformSecurityPolicy.
// +kubebuilder:object:root=true
type PlatformSecurityPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformSecurityPolicy `json:"items"`
}

const (
	PlatformSecurityPolicyName      = "default"
	PlatformSecurityConfigMapName   = "gentian-platform-security"
	PlatformSecurityConfigMapKey    = "allowedMacWaivers.json"
	PlatformSecurityConfigTypeLabel = "platform-security"
)

func init() {
	SchemeBuilder.Register(&PlatformSecurityPolicy{}, &PlatformSecurityPolicyList{})
}
