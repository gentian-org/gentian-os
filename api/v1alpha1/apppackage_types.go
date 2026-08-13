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

// AppPackage is a named preset that pre-selects a set of addons for a family.
//
// It is deliberately **not deployable**. Nothing reconciles an AppPackage and it
// owns no workload, namespace or credential: it exists only so the App Store can
// offer "Suite" as one click instead of nine checkboxes. Choosing a package
// pre-ticks its addons in the selection window; the user is free to adjust the
// selection before installing, and to change it afterwards via Edit.
//
// This is what replaced the old package *profiles* (nextcloud drive/office/suite),
// which baked a fixed plugin set into an image and so allowed exactly four
// combinations. Making the curated set a preset rather than an artifact keeps the
// curation benefit without constraining what a tenant can actually install.
//
// See gentian-os/docs/app-customization.md §4.2.

// AppPackageSpec is the desired state of an AppPackage.
type AppPackageSpec struct {
	// Family is the app family this preset belongs to (matches AppProfile.spec.family).
	// The App Store only offers the preset while installing an app of that family.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Family string `json:"family"`

	// DisplayName is the name shown in the App Store (e.g. "Nextcloud Suite").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description explains what the preset bundles, for the selection window.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`

	// Addons lists the AppProfile names this preset pre-selects. Each must name an
	// addon profile (gentianos.io/deployment-role: addon) in the same family.
	//
	// Entries are a starting point, not a constraint: the user may untick any of
	// them, and may add addons the preset does not list. A preset that names an
	// addon the tenant is not entitled to still renders — with a Buy button rather
	// than a tick — so a package can advertise an ee addon without gating install.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Addons []string `json:"addons"`

	// Author is who supplies this preset — a company, an organisation, or an
	// individual. Same meaning as AppProfile.spec.author.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Author string `json:"author,omitempty"`

	// Tile is the App Store icon for the preset.
	// +optional
	Tile *TileSpec `json:"tile,omitempty"`
}

// AppPackage is a named addon preset for a family. Presentation only — see the
// type comment above for why it carries no status and no reconciler.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=apkg;apkgs
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.family`
// +kubebuilder:printcolumn:name="Addons",type=string,JSONPath=`.spec.addons`
// +kubebuilder:printcolumn:name="Author",type=string,JSONPath=`.spec.author`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppPackage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AppPackageSpec `json:"spec,omitempty"`
}

// AppPackageList contains a list of AppPackage.
// +kubebuilder:object:root=true
type AppPackageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppPackage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppPackage{}, &AppPackageList{})
}
