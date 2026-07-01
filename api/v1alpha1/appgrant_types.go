// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppGrantSpec defines tenant-approved integration permissions for one installed app.
type AppGrantSpec struct {
	// App is the installed AppProfile name (matches Tenant.spec.apps[].profile).
	// +kubebuilder:validation:Required
	App string `json:"app"`

	// Consume lists contract capabilities this app may use from providers in the tenant.
	// Each entry must be a subset of AppProfile.spec.optionalIntegrations.
	// +optional
	Consume []ConsumeGrantSpec `json:"consume,omitempty"`

	// AllowConsumers controls which other installed apps may invoke this app's provides contracts.
	// +optional
	AllowConsumers []AllowConsumerSpec `json:"allowConsumers,omitempty"`
}

// ConsumeGrantSpec is the tenant-approved consumption grant for one contract.
type ConsumeGrantSpec struct {
	// Contract matches IntegrationBinding.spec.contract.
	// +kubebuilder:validation:Required
	Contract string `json:"contract"`

	// Granted lists approved capability strings (subset of the binding declaration).
	// +kubebuilder:validation:MinItems=1
	Granted []string `json:"granted"`
}

// AllowConsumerSpec permits another installed app to call a provided contract.
type AllowConsumerSpec struct {
	// App is the consumer AppProfile name.
	// +kubebuilder:validation:Required
	App string `json:"app"`

	// Contract matches AppProfile.spec.provides[].name.
	// +kubebuilder:validation:Required
	Contract string `json:"contract"`

	// Scope lists approved capability strings for the consumer.
	// +kubebuilder:validation:MinItems=1
	Scope []string `json:"scope"`
}

// AppGrantPhase summarises grant reconciliation.
// +kubebuilder:validation:Enum=Pending;Ready;Degraded
type AppGrantPhase string

const (
	AppGrantPhasePending  AppGrantPhase = "Pending"
	AppGrantPhaseReady    AppGrantPhase = "Ready"
	AppGrantPhaseDegraded AppGrantPhase = "Degraded"
)

// AppGrantStatus holds observed grant state.
type AppGrantStatus struct {
	// Phase summarises OpenFGA tuple sync.
	// +optional
	Phase AppGrantPhase `json:"phase,omitempty"`

	// Conditions provide detailed status.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled spec generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// TupleCount is the number of OpenFGA tuples managed for this grant.
	// +optional
	TupleCount int `json:"tupleCount,omitempty"`
}

// AppGrant is a tenant-scoped authorization grant — a subset of AppProfile declarations.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag;grant
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppGrantSpec   `json:"spec,omitempty"`
	Status AppGrantStatus `json:"status,omitempty"`
}

// AppGrantList contains a list of AppGrant.
// +kubebuilder:object:root=true
type AppGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppGrant{}, &AppGrantList{})
}
