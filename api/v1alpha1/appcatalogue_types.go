/*
Copyright 2026 The Gentian Authors.

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

// AppCatalogue is a singleton cluster-scoped resource maintained by the
// App Store controller. It reflects all AppProfile CRs present in the cluster
// and cross-references them with Tenant installations.
//
// Only one instance should exist; it is always named "default".
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=appcat
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Apps",type=integer,JSONPath=`.status.totalApps`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppCatalogue struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppCatalogueSpec   `json:"spec,omitempty"`
	Status AppCatalogueStatus `json:"status,omitempty"`
}

// AppCatalogueSpec is intentionally empty — the catalogue is fully status-driven.
type AppCatalogueSpec struct{}

// AppCatalogueStatus holds the observed catalogue state.
type AppCatalogueStatus struct {
	// Apps lists every available AppProfile with installation metadata.
	// +optional
	// +listType=map
	// +listMapKey=name
	Apps []CatalogueEntry `json:"apps,omitempty"`

	// TotalApps is the count of available AppProfile CRs.
	// +optional
	TotalApps int `json:"totalApps,omitempty"`

	// LastUpdated is the time the catalogue was last successfully reconciled.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// CatalogueEntry summarises one AppProfile for catalogue consumers.
type CatalogueEntry struct {
	// Name is the AppProfile CR name.
	Name string `json:"name"`

	// DisplayName is the human-readable application name.
	DisplayName string `json:"displayName"`

	// Description is the optional human-readable description.
	// +optional
	Description string `json:"description,omitempty"`

	// ChartVersion is the chart version declared in the AppProfile.
	ChartVersion string `json:"chartVersion"`

	// DeploymentMethod is the deployment pattern (argocd or tofu-controller).
	DeploymentMethod DeploymentMethod `json:"deploymentMethod"`

	// KernelRequirements is a compact human-readable list of kernel services the
	// app requires (e.g., ["oidc", "postgresql", "s3", "smtp", "ldap"]).
	// +optional
	KernelRequirements []string `json:"kernelRequirements,omitempty"`

	// InstalledCount is the number of Tenant CRs that currently list this app.
	InstalledCount int `json:"installedCount"`
}

// AppCatalogueList contains a list of AppCatalogue.
// +kubebuilder:object:root=true
type AppCatalogueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppCatalogue `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppCatalogue{}, &AppCatalogueList{})
}
