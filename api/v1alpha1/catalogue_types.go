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
	AnnotationProfileDeploymentRole = "gentianos.io/deployment-role"
	// GatewayRootRedirect is an HTTPRoute redirect target for GET / on the app host.
	AnnotationProfileGatewayRootRedirect = "gentianos.io/gateway-root-redirect"
	// GatewayAPIBackends is a JSON array of extra path→Service routes on the app host.
	// Shape: [{"pathPrefix":"/api","serviceName":"my-api","port":8080}]
	AnnotationProfileGatewayAPIBackends = "gentianos.io/gateway-api-backends"
	// OIDCDefaultRedirectURIs is a JSON array used when spec.kernelRequirements.identity.oidc.redirectUris is empty.
	// Supports ${TENANT_DOMAIN} substitution.
	AnnotationProfileOIDCDefaultRedirectURIs = "gentianos.io/oidc-default-redirect-uris"
	// KernelEgressNamespaces is a comma-separated list of extra cluster namespaces the
	// app workload may reach (merged into kernel-access NetworkPolicy egress).
	AnnotationProfileKernelEgressNamespaces = "gentianos.io/kernel-egress-namespaces"
)

// Per-host gateway policy annotations on IngressSpec.annotations (primary or additionalIngresses).
const (
	// GatewayFrameAncestors is JSON: {"mode":"replace|append","origins":["portal","mainApp",...]}.
	AnnotationIngressGatewayFrameAncestors = "gentianos.io/gateway-frame-ancestors"
	// GatewayEscapedSlashesAction sets Envoy ClientTrafficPolicy path.escapedSlashesAction.
	AnnotationIngressGatewayEscapedSlashesAction = "gentianos.io/gateway-escaped-slashes-action"
	// GatewayRequestTimeout sets BackendTrafficPolicy timeout.http.requestTimeout (e.g. "3600s" or "3600").
	AnnotationIngressGatewayRequestTimeout = "gentianos.io/gateway-request-timeout"
	// GatewayResponseTimeout sets BackendTrafficPolicy timeout.http.responseTimeout.
	AnnotationIngressGatewayResponseTimeout = "gentianos.io/gateway-response-timeout"
	// GatewayBufferLimit sets BackendTrafficPolicy connection.bufferLimit (e.g. "128m").
	AnnotationIngressGatewayBufferLimit = "gentianos.io/gateway-buffer-limit"
)

// ProfileDeploymentRole describes how a catalogue entry is deployed relative to siblings.
type ProfileDeploymentRole string

const (
	ProfileDeploymentRoleStandalone ProfileDeploymentRole = "standalone"
	ProfileDeploymentRoleBase       ProfileDeploymentRole = "base"
	// ProfileDeploymentRoleAddon marks a profile activated inside a base rather than
	// deployed on its own — customization-ladder rung L3. "addon" is the single word
	// for this across all apps; upstream may call them modules (Odoo) or apps
	// (Nextcloud), but that is their vocabulary, not ours.
	ProfileDeploymentRoleAddon ProfileDeploymentRole = "addon"
	// ProfileDeploymentRoleModule is the pre-cleanup spelling of Addon, still accepted
	// on input so the catalogue can migrate without a flag day. Deprecated: use Addon.
	ProfileDeploymentRoleModule ProfileDeploymentRole = "module"
)

// Default catalogue values applied when fields are omitted on legacy profiles.
const (
	DefaultCatalogueVersion = "1.0.0"
)

// Edition identifies which edition of an app a profile packages:
//
//	ce — community edition, as published by the upstream organisation
//	me — maintained edition: ce plus active maintenance by Gentian
//	ee — commercially licensed, entitlement-gated; supplied either by the upstream
//	     organisation or by a third party (see AppProfile.spec.author)
//
// Editions are technically interchangeable; what decides whether one may run is
// authorization (a paid licence), and technical addon/base compatibility is managed
// by version. See gentian-os/docs/app-customization.md §4.2.
//
// +kubebuilder:validation:Enum=ce;me;ee
type Edition string

const (
	// EditionCE is the community edition as published by the upstream organisation.
	EditionCE Edition = "ce"
	// EditionME is the community edition plus active maintenance by Gentian.
	EditionME Edition = "me"
	// EditionEE is a commercially licensed edition requiring an entitlement. It says
	// the entry is paid-for, not who publishes it: spec.author names the supplier,
	// which may be the upstream organisation or a third party packaging it.
	EditionEE Edition = "ee"
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
	// Family is the stable logical application id (e.g. "demo-app").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Family string `json:"family"`

	// CatalogueVersion is the semver of this catalogue entry.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?(?:\+[\w.-]+)?$`
	CatalogueVersion string `json:"catalogueVersion"`

	// Edition selects the edition (ce, me, ee).
	// +optional
	// +kubebuilder:default=ce
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
