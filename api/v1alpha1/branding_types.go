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
	"k8s.io/apimachinery/pkg/runtime"
)

// BrandingName is the one Branding a cluster reads.
const BrandingName = "default"

// BrandIdentity is what the brand is called and what it looks like as an
// app: the Web App Manifest's own members, so the identity a browser installs
// and the one the pages show are described the same way.
type BrandIdentity struct {
	// Name is the product's name, on sign-in cards, page titles and the
	// sender of the identity provider's mail.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name,omitempty"`

	// ShortName is where Name does not fit: an installed app's label.
	// +optional
	// +kubebuilder:validation:MaxLength=24
	ShortName string `json:"shortName,omitempty"`

	// Description is the installed app's description.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`

	// Icons are the logo and favicon, as a manifest's icons: the first whose
	// purpose includes "any" is the logo on the pages.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Icons []BrandIcon `json:"icons,omitempty"`
}

// BrandIcon is one Web App Manifest image resource.
type BrandIcon struct {
	// Src is an https URL, or the image itself as a data URL. A data URL keeps
	// the brand in the cluster's own repository and asks no third party
	// whenever a page is shown.
	// +kubebuilder:validation:MaxLength=262144
	// +kubebuilder:validation:Pattern=`^(https://[^\s"'<>]+|data:image/(png|svg\+xml|webp|x-icon|vnd\.microsoft\.icon);base64,[A-Za-z0-9+/=]+)$`
	Src string `json:"src"`

	// Sizes, as the manifest says them: "any", or "192x192 512x512".
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(any|[0-9]+x[0-9]+( [0-9]+x[0-9]+)*)?$`
	Sizes string `json:"sizes,omitempty"`

	// Type is the image's media type.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Type string `json:"type,omitempty"`

	// Purpose, as the manifest says it: any, maskable, monochrome.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^((any|maskable|monochrome)( (any|maskable|monochrome))*)?$`
	Purpose string `json:"purpose,omitempty"`
}

// BrandingSpec is the cluster's brand.
type BrandingSpec struct {
	// Identity is the product's name and icons.
	// +optional
	Identity BrandIdentity `json:"identity,omitempty"`

	// Tokens is the brand's look: a design-token document in the W3C Design
	// Tokens Community Group format (2025.10). Pages read color.brand.*,
	// color.ink.*, color.paper.*, color.status.*, font.family.* and radius.*,
	// and keep their own value for a token that is absent.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Tokens *runtime.RawExtension `json:"tokens,omitempty"`

	// HideVendorPromotions takes the platform vendor's own offers off the
	// consoles, for a provider who runs the cluster under its own name.
	// +optional
	HideVendorPromotions bool `json:"hideVendorPromotions,omitempty"`
}

// BrandingStatus says whether the brand is what the pages show.
type BrandingStatus struct {
	// Conditions: Rendered is True once the pages' stylesheet is the brand's,
	// False with the reason when the tokens cannot be rendered.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation Rendered describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Branding is the brand every page on the cluster shows: the sign-in router,
// the identity provider's screens, the desktop and the consoles. One per
// cluster, named "default"; without it they show the platform's own.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="the Branding is a singleton named 'default', the name the operator reads"
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.identity.name`
// +kubebuilder:printcolumn:name="Rendered",type=string,JSONPath=`.status.conditions[?(@.type=="Rendered")].status`
type Branding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BrandingSpec   `json:"spec,omitempty"`
	Status BrandingStatus `json:"status,omitempty"`
}

// BrandingList contains a list of Branding.
// +kubebuilder:object:root=true
type BrandingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Branding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Branding{}, &BrandingList{})
}
