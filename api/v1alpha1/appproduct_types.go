package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppProductSpec defines the desired state of AppProduct.
type AppProductSpec struct {
	// DisplayName is the human-readable product name shown in the store.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Description is an optional human-readable description.
	// +optional
	Description string `json:"description,omitempty"`

	// Logo is an optional data URI (data:image/svg+xml;base64,...) for the store tile.
	// +optional
	// +kubebuilder:validation:Pattern=`^data:image/svg\+xml;base64,[A-Za-z0-9+/]+=*$`
	Logo string `json:"logo,omitempty"`

	// CatalogueVersion is the semver of this store SKU listing.
	// Bump when the product bundle, pricing tier, or pinned profiles change.
	// +optional
	// +kubebuilder:default="1.0.0"
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?(?:\+[\w.-]+)?$`
	CatalogueVersion string `json:"catalogueVersion,omitempty"`

	// Edition is the default feature / footprint variant for this SKU.
	// Individual profileRefs may override via identity.edition.
	// +optional
	// +kubebuilder:default=full
	Edition Edition `json:"edition,omitempty"`

	// OfferingTier is the commercial / support tier for this SKU (pricing axis).
	// +optional
	// +kubebuilder:default=free
	OfferingTier OfferingTier `json:"offeringTier,omitempty"`

	// TrustTier is the catalogue certification tier (security / trust axis).
	// Distinct from offeringTier (commercial pricing).
	// +optional
	// +kubebuilder:default=certified
	TrustTier TrustTier `json:"trustTier,omitempty"`

	// ProfileRefs lists one or more AppProfile revisions to install when this product
	// is checked out. Each entry resolves by name or by ProfileIdentity.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	ProfileRefs []ProfileReference `json:"profileRefs"`

	// Publisher identifies who publishes this store listing.
	// +kubebuilder:validation:Required
	Publisher ProductPublisher `json:"publisher"`

	// Listable controls whether the product appears in the app store catalogue.
	// Defaults to true.
	// +optional
	// +kubebuilder:default=true
	Listable *bool `json:"listable,omitempty"`
}

// AppProduct is the Schema for the appproducts API.
//
// AppProduct is cluster-scoped — one per sellable SKU in the store. It references
// one or more immutable AppProfile revisions and carries commercial metadata
// (offering tier, trust tier, catalogue version) separate from technical deploy config.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=appprod;aprod
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Cat.Version",type=string,JSONPath=`.spec.catalogueVersion`
// +kubebuilder:printcolumn:name="Edition",type=string,JSONPath=`.spec.edition`
// +kubebuilder:printcolumn:name="Offering",type=string,JSONPath=`.spec.offeringTier`
// +kubebuilder:printcolumn:name="Trust",type=string,JSONPath=`.spec.trustTier`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppProduct struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AppProductSpec `json:"spec,omitempty"`
}

// AppProductList contains a list of AppProduct.
// +kubebuilder:object:root=true
type AppProductList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppProduct `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppProduct{}, &AppProductList{})
}
