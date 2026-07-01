package v1alpha1

// Catalogue label keys index AppProfile revisions by logical identity.
// The App Store controller ensures these labels are present on every profile.
const (
	LabelProfileName             = "gentianos.io/profile-name"
	LabelProfileFamily           = "gentianos.io/profile-family"
	LabelProfileCatalogueVersion = "gentianos.io/profile-catalogue-version"
	LabelProfileEdition          = "gentianos.io/profile-edition"
	LabelProfileTrustTier        = "gentianos.io/profile-trust-tier"
)

// Profile bundle annotations — generic catalogue semantics without CRD fields per app.
// App-specific install parameters belong in extraValues or profile-scoped compositions.
const (
	AnnotationProfileDeploymentRole  = "gentianos.io/deployment-role"
	AnnotationProfileRequiresProfile = "gentianos.io/requires-profile"
	// GatewayRootRedirect is an HTTPRoute redirect target for GET / on the app host.
	AnnotationProfileGatewayRootRedirect = "gentianos.io/gateway-root-redirect"
	// GatewayAPIBackends is a JSON array of extra path→Service routes on the app host.
	// Shape: [{"pathPrefix":"/api","serviceName":"my-api","port":8080}]
	AnnotationProfileGatewayAPIBackends = "gentianos.io/gateway-api-backends"
	// OIDCDefaultRedirectURIs is a JSON array used when spec.kernelRequirements.identity.oidc.redirectUris is empty.
	// Supports ${TENANT_DOMAIN} substitution (legacy pack Jobs and operator fallbacks).
	AnnotationProfileOIDCDefaultRedirectURIs = "gentianos.io/oidc-default-redirect-uris"
)

// Per-host gateway policy annotations on IngressSpec.annotations (primary or additionalIngresses).
const (
	// GatewayFrameAncestors is JSON: {"mode":"replace|append","origins":["portal","mainApp",...]}.
	AnnotationIngressGatewayFrameAncestors = "gentianos.io/gateway-frame-ancestors"
	// GatewayEscapedSlashesAction sets Envoy ClientTrafficPolicy path.escapedSlashesAction.
	AnnotationIngressGatewayEscapedSlashesAction = "gentianos.io/gateway-escaped-slashes-action"
)

// ProfileDeploymentRole describes how a catalogue entry is deployed relative to siblings.
type ProfileDeploymentRole string

const (
	ProfileDeploymentRoleStandalone ProfileDeploymentRole = "standalone"
	ProfileDeploymentRoleBase       ProfileDeploymentRole = "base"
	ProfileDeploymentRoleModule     ProfileDeploymentRole = "module"
)

// Default catalogue values applied when fields are omitted on legacy profiles.
const (
	DefaultCatalogueVersion = "1.0.0"
)

// Edition describes the feature / footprint variant of a profile.
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

// TrustTier describes platform certification / review level for a catalogue entry.
//
// +kubebuilder:validation:Enum=platform;certified;experimental
type TrustTier string

const (
	TrustTierPlatform     TrustTier = "platform"
	TrustTierCertified    TrustTier = "certified"
	TrustTierExperimental TrustTier = "experimental"
)

// ProfileIdentity uniquely identifies an immutable AppProfile catalogue revision
// within the tuple (family, catalogueVersion, edition).
type ProfileIdentity struct {
	// Family is the stable logical application id (e.g. "openproject").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Family string `json:"family"`

	// CatalogueVersion is the semver of this catalogue entry.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?(?:\+[\w.-]+)?$`
	CatalogueVersion string `json:"catalogueVersion"`

	// Edition selects the feature / footprint variant.
	// +optional
	// +kubebuilder:default=full
	Edition Edition `json:"edition,omitempty"`
}

// ProfileReference resolves to exactly one AppProfile CR — either by metadata.name
// (explicit pin) or by ProfileIdentity (dimensional selector).
type ProfileReference struct {
	// Name is the AppProfile metadata.name. When set, this is the exact profile CR to use.
	// +optional
	Name string `json:"name,omitempty"`

	// Identity selects a profile by catalogue tuple when Name is empty.
	// +optional
	Identity *ProfileIdentity `json:"identity,omitempty"`
}
