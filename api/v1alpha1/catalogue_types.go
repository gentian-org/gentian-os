package v1alpha1

// Catalogue label keys index AppProfile revisions by logical identity.
// The App Store controller ensures these labels are present on every profile.
const (
	LabelProfileName            = "gentianos.io/profile-name"
	LabelProfileFamily          = "gentianos.io/profile-family"
	LabelProfileCatalogueVersion = "gentianos.io/profile-catalogue-version"
	LabelProfileEdition         = "gentianos.io/profile-edition"
	LabelProfileOfferingTier    = "gentianos.io/profile-offering-tier"
	LabelProductName            = "gentianos.io/product-name"
)

// Default catalogue values applied when fields are omitted on legacy profiles.
const (
	DefaultCatalogueVersion = "1.0.0"
)

// Edition describes the feature / footprint variant of a profile or product SKU.
// Examples: minimal (reduced components), full (all integrations), performant (scaled resources).
//
// +kubebuilder:validation:Enum=minimal;standard;full;performant
type Edition string

const (
	EditionMinimal    Edition = "minimal"
	EditionStandard   Edition = "standard"
	EditionFull       Edition = "full"
	EditionPerformant Edition = "performant"
)

// OfferingTier describes the commercial / support axis (pricing, SLA, hardening pack).
// Distinct from TrustTier (catalogue certification).
//
// +kubebuilder:validation:Enum=free;hardened;supported
type OfferingTier string

const (
	OfferingTierFree      OfferingTier = "free"
	OfferingTierHardened  OfferingTier = "hardened"
	OfferingTierSupported OfferingTier = "supported"
)

// TrustTier describes catalogue trust / certification for store listings.
// Distinct from OfferingTier (commercial pricing).
//
// +kubebuilder:validation:Enum=platform;certified;experimental
type TrustTier string

const (
	TrustTierPlatform      TrustTier = "platform"
	TrustTierCertified     TrustTier = "certified"
	TrustTierExperimental  TrustTier = "experimental"
)

// ProfileIdentity uniquely identifies an immutable AppProfile catalogue revision
// within the tuple (family, catalogueVersion, edition, offeringTier).
// Family groups all versions and flavors of one logical application.
type ProfileIdentity struct {
	// Family is the stable logical application id (e.g. "openproject").
	// All catalogue revisions of the same app share a family.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Family string `json:"family"`

	// CatalogueVersion is the semver of this catalogue entry.
	// Immutable once published — bump to ship a new revision.
	// Distinct from spec.chart.version (upstream Helm chart pin).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?(?:\+[\w.-]+)?$`
	CatalogueVersion string `json:"catalogueVersion"`

	// Edition selects the feature / footprint variant.
	// +optional
	// +kubebuilder:default=full
	Edition Edition `json:"edition,omitempty"`

	// OfferingTier selects the commercial / support variant.
	// +optional
	// +kubebuilder:default=free
	OfferingTier OfferingTier `json:"offeringTier,omitempty"`
}

// ProfileReference resolves to exactly one AppProfile CR — either by metadata.name
// (legacy / explicit pin) or by ProfileIdentity (dimensional selector).
// At least one of Name or Identity must be set; Name takes precedence when both are present.
type ProfileReference struct {
	// Name is the AppProfile metadata.name. When set, this is the exact profile CR to use.
	// +optional
	Name string `json:"name,omitempty"`

	// Identity selects a profile by catalogue tuple when Name is empty.
	// +optional
	Identity *ProfileIdentity `json:"identity,omitempty"`
}

// ProductPublisher identifies who publishes a store listing.
type ProductPublisher struct {
	// Name is the human-readable publisher name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// URL is an optional link to the publisher homepage or support portal.
	// +optional
	URL string `json:"url,omitempty"`
}
