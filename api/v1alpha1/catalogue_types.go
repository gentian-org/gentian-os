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

// Catalogue label keys index ComponentProfile revisions by logical identity.
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
	// OIDCDefaultRedirectURIs is a JSON array used when spec.requires.services.identity.oidc.redirectUris is empty.
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
//	ce — community edition: open source, as published by the upstream organisation
//	me — maintained edition: the community edition's code, curated and
//	     maintained by the catalogue's maintainer, who runs it itself
//	ie — internal edition: somebody's own profile, in their own catalogue
//	pe — proprietary edition: proprietary, free of charge
//	ee — enterprise edition: proprietary and paid for
//
// An edition describes who maintains and supports an entry. The five are
// technically interchangeable and the platform gates none of them: whether a
// paid entry arrives is decided at the repository it is pulled from, by the
// credential the tenant holds for it. Addon/base compatibility is managed by
// version. See gentian-os/docs/app-customization.md §4.2.
//
// An me entry packages the community edition's code and is named -ce like
// it: the edition is what the profile's gentianos.io/profile-edition label
// says, never a name's suffix.
//
// The split that matters operationally is not free against paid but WHERE the
// entry comes from. ce, me and ie are entries a cluster can hold and install
// on its own: ce and me because their code is public, ie because it is the
// operator's own. pe and ee are somebody's proprietary product, offered
// through the App Store, and a cluster browsing its own catalogue does not
// list them (AD-14) -- there is nothing useful it could say about an entry
// whose whole value is a relationship with a supplier.
//
// +kubebuilder:validation:Enum=ce;me;ie;pe;ee
type Edition string

const (
	// EditionCE is the community edition as published by the upstream organisation.
	EditionCE Edition = "ce"
	// EditionME is the community edition's code, curated and maintained by
	// the catalogue's maintainer, who runs it itself and so stands behind it.
	EditionME Edition = "me"
	// EditionIE is an internal edition: a profile its own operator wrote and
	// publishes in their own catalogue source, for their own tenants. Nobody
	// sells it and nobody else lists it, so nothing outside the cluster needs
	// to be reachable for it to be installed -- which is exactly why the
	// cluster's own catalogue view exists.
	EditionIE Edition = "ie"
	// EditionPE is the proprietary edition: a supplier's proprietary product,
	// free of charge. spec.author names the supplier.
	EditionPE Edition = "pe"
	// EditionEE is the enterprise edition: proprietary and paid for, and
	// maintained and supported by its supplier. spec.author names the
	// supplier, which may be the upstream organisation or a third party
	// packaging it. Nothing in the platform gates it; its supply is
	// controlled at the repository it is pulled from.
	EditionEE Edition = "ee"
)

// LocalEditions are the editions a cluster lists from its own catalogue
// sources. See Edition.
var LocalEditions = []Edition{EditionCE, EditionME, EditionIE}

// Local reports whether an edition is one a cluster lists for itself.
func (e Edition) Local() bool {
	return e == EditionCE || e == EditionME || e == EditionIE
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

// ProfileIdentity uniquely identifies an immutable ComponentProfile catalogue revision
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

	// Edition selects the edition (ce, me, ie, pe, ee).
	// +optional
	// +kubebuilder:default=ce
	Edition Edition `json:"edition,omitempty"`
}

// ProfileReference resolves to exactly one ComponentProfile — either by metadata.name
// (explicit pin) or by ProfileIdentity (dimensional selector).
type ProfileReference struct {
	// Name is the ComponentProfile metadata.name. When set, this is the exact profile CR to use.
	// +optional
	Name string `json:"name,omitempty"`

	// Identity selects a profile by catalogue tuple when Name is empty.
	// +optional
	Identity *ProfileIdentity `json:"identity,omitempty"`
}
