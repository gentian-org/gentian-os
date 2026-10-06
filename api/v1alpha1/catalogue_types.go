/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
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

	// AnnotationProfileRequiresEntitlement marks an entry that must be paid
	// for before it may be activated. It is an annotation rather than a spec
	// field because AD-3 moves the licence itself to the store's listing,
	// outside the cluster — but whether something needs paying for is an
	// authorization question and the thing that enforces it runs in here.
	AnnotationProfileRequiresEntitlement = "gentianos.io/requires-entitlement"

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
	// GatewayRequestTimeout sets BackendTrafficPolicy timeout.http.requestTimeout
	// (e.g. "3600s" or "3600"). This is the whole budget for the exchange, not
	// just for sending the request -- Envoy holds one route timeout covering the
	// upstream's response as well.
	//
	// There is deliberately no response-timeout companion. One existed, as
	// gentianos.io/gateway-response-timeout, and did nothing at all:
	// BackendTrafficPolicy's timeout.http has only connectionIdleTimeout,
	// maxConnectionDuration and requestTimeout, so the API server pruned the
	// field on write and every policy came back carrying requestTimeout alone.
	// Four profiles set it, all to the same value as the request timeout, and
	// none of them ever got what the name promised. Nothing tested it, which is
	// why it stayed that way.
	AnnotationIngressGatewayRequestTimeout = "gentianos.io/gateway-request-timeout"
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
)

// Default catalogue values applied when fields are omitted on legacy profiles.
const (
	DefaultCatalogueVersion = "1.0.0"
)

// Edition identifies which edition of an app a profile packages, and with it
// who stands behind the entry:
//
//	ce — community edition, as published by the upstream organisation
//	pe — private edition: somebody's own profile, in their own catalogue
//	me — maintained edition: ce plus active maintenance by Gentian
//	ee — enterprise edition: commercially licensed, entitlement-gated
//
// The four are technically interchangeable; what decides whether one may run
// is authorization, and addon/base compatibility is managed by version. See
// gentian-os/docs/app-customization.md §4.2.
//
// The split that matters operationally is not free against paid but WHERE the
// entry comes from. ce and pe are entries a cluster can hold and install on
// its own: ce because it is public, pe because it is the operator's own. me
// and ee exist because somebody maintains or licenses them, which is the App
// Store's business, and a cluster browsing its own catalogue does not list
// them (AD-14) — there is nothing useful it could say about an entry whose
// whole value is a relationship with a supplier.
//
// +kubebuilder:validation:Enum=ce;pe;me;ee
type Edition string

const (
	// EditionCE is the community edition as published by the upstream organisation.
	EditionCE Edition = "ce"
	// EditionPE is a private edition: a profile its own operator wrote and
	// publishes in their own catalogue source, for their own tenants. Nobody
	// sells it and nobody else lists it, so nothing outside the cluster needs
	// to be reachable for it to be installed -- which is exactly why the
	// cluster's own catalogue view exists.
	EditionPE Edition = "pe"
	// EditionME is the community edition plus active maintenance by Gentian:
	// the editions Gentian Technologies itself runs and keeps current.
	EditionME Edition = "me"
	// EditionEE is the enterprise edition: commercially licensed and requiring
	// an entitlement. It says the entry is paid-for, not who publishes it --
	// spec.author names the supplier, which may be the upstream organisation
	// or a third party packaging it.
	EditionEE Edition = "ee"
)

// LocalEditions are the editions a cluster lists from its own catalogue
// sources. See Edition.
var LocalEditions = []Edition{EditionCE, EditionPE}

// Local reports whether an edition is one a cluster lists for itself.
func (e Edition) Local() bool {
	return e == EditionCE || e == EditionPE
}

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
