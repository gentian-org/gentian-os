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

// AppProfileSpec defines the desired state of AppProfile.
// An addon is activated inside a base rather than deployed, so it has no chart of
// its own. The rule tests spec.customization.addon rather than the deployment-role
// annotation because CEL on spec cannot reach metadata.
// +kubebuilder:validation:XValidation:rule="self.deploymentMethod == 'api' || has(self.chart) || (has(self.customization) && has(self.customization.addon))",message="spec.chart is required unless spec.deploymentMethod is api or the profile declares spec.customization.addon"
// +kubebuilder:validation:XValidation:rule="self.deploymentMethod != 'api' || has(self.apiIntegration)",message="spec.apiIntegration is required when spec.deploymentMethod is api"
type AppProfileSpec struct {
	// DisplayName is a human-readable name for the application.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// Family is the stable logical application id shared across catalogue revisions
	// (versions and flavors). When empty the App Store controller defaults it to
	// metadata.name. Use the same family for every immutable revision of one app.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Family string `json:"family,omitempty"`

	// CatalogueVersion is the semver of this catalogue entry (immutable once published).
	// Distinct from spec.chart.version (upstream Helm chart pin).
	// +optional
	// +kubebuilder:default="1.0.0"
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?(?:\+[\w.-]+)?$`
	CatalogueVersion string `json:"catalogueVersion,omitempty"`

	// Edition selects the edition: ce (community, as upstream publishes it), me
	// (ce plus active Gentian maintenance) or ee (commercially licensed and
	// entitlement-gated, supplied by whoever spec.author names).
	// +optional
	// +kubebuilder:default=ce
	Edition Edition `json:"edition,omitempty"`

	// TrustTier is the platform certification level for this catalogue entry.
	// +optional
	// +kubebuilder:default=certified
	TrustTier TrustTier `json:"trustTier,omitempty"`

	// License is the SPDX license identifier for this catalogue entry (e.g. Apache-2.0).
	// Entries requiring a paid entitlement use proprietary; see
	// ProfileRequiresEntitlement.
	// +optional
	License string `json:"license,omitempty"`

	// Author is who supplies and maintains *this* catalogue entry — a company
	// (vendor), an organisation, or an individual. It describes the entry, not the
	// upstream project: the same application can appear as several profiles from
	// different authors, e.g. a "ce" edition packaged by Gentian alongside an "ee"
	// edition supplied by a third-party vendor.
	//
	// This is why the edition must never be inferred from a profile's name. Two
	// profiles may both be edition "ee" from different authors and carry entirely
	// unrelated names; spec.edition and spec.author are the authoritative pair, the
	// name is only a hint. See gentian-os/docs/app-customization.md §4.2.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Author string `json:"author,omitempty"`

	// Categories lists the classification categories for this application.
	// +optional
	Categories []string `json:"categories,omitempty"`

	// Keywords lists search keywords for this application.
	// +optional
	Keywords []string `json:"keywords,omitempty"`

	// Description is an optional human-readable description.
	// +optional
	Description string `json:"description,omitempty"`

	// Tile declares the Gentian portal app-menu icon for this profile.
	// Path 1 (custom): set tile.logo to a data URI or tile.image in git (inlined before publish).
	// Path 2 (catalogue): set tile.icon to a Gentian catalogue id (e.g. "mail", "chat").
	// +optional
	Tile *TileSpec `json:"tile,omitempty"`

	// BrowserProxy declares HTTP routes the shell backend should transparently
	// proxy on behalf of browser clients. Each route is exposed at
	// /api/apps/{appName}/{route.path} on the shell server and forwarded to
	// the app's cluster-internal service. Use this to give the shell CORS-safe
	// access to an app's API without embedding app credentials in the browser.
	// Supports {name} (app profile name) and {namespace} (tenant namespace)
	// as runtime template variables in the target field.
	// +optional
	BrowserProxy []BrowserProxyRoute `json:"browserProxy,omitempty"`

	// KernelRequirements declares which kernel services this app requires.
	// +optional
	KernelRequirements *KernelRequirements `json:"kernelRequirements,omitempty"`

	// Provides lists the integration contracts this app can act as a provider for.
	// +optional
	Provides []ContractRef `json:"provides,omitempty"`

	// OptionalIntegrations lists contracts this app can consume when available.
	// +optional
	OptionalIntegrations []IntegrationRef `json:"optionalIntegrations,omitempty"`

	// Chart references the upstream Helm chart for this app. Required for
	// crossplane and argocd deployment methods; omitted for ApiProfiles
	// (deploymentMethod: api), which run no workload. Enforced by a spec-level
	// validation rule.
	// +optional
	Chart ChartRef `json:"chart,omitempty"`

	// ValueMapping is the schema-based mapping of kernel-provided values to
	// Helm value keys. Validated at admission time.
	// +optional
	ValueMapping *ValueMapping `json:"valueMapping,omitempty"`

	// AppSecrets declares app-internal secrets (admin passwords, session keys,
	// cluster tokens) that don't correspond to any kernel function. The
	// orchestrator generates these deterministically (HMAC-SHA256 from master
	// password + tenant + app + secret name), stores them in OpenBao, and syncs
	// them via ExternalSecret.
	// +optional
	AppSecrets []AppSecret `json:"appSecrets,omitempty"`

	// DerivedSecretKeys declares extra deterministic values to place in the
	// credentials Secret the platform manages for this app, which the app
	// typically consumes with envFrom.
	//
	// Prefer appSecrets: those are HMAC'd from the master password and stored in
	// OpenBao. This is the weaker, legacy carrier — a local SHA-256 with no
	// OpenBao record — and exists so that apps already depending on such a value
	// can declare it instead of the platform hardcoding their name.
	// +optional
	DerivedSecretKeys []DerivedSecretKey `json:"derivedSecretKeys,omitempty"`

	// ExtraValues provides an escape hatch for non-standard values that don't
	// fit the typed valueMapping schema. Merged into the rendered Helm values.
	// Must not contain secrets — use appSecrets for secret values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`

	// DeploymentMethod controls how the orchestrator deploys this app.
	// Defaults to crossplane. Use argocd for kernel-layer services managed
	// directly by the cache or identity reconcilers. Use api for an ApiProfile
	// that runs no workload (see spec.apiIntegration).
	// +optional
	// +kubebuilder:default=crossplane
	DeploymentMethod DeploymentMethod `json:"deploymentMethod,omitempty"`

	// APIIntegration configures an ApiProfile (deploymentMethod: api): a
	// catalogue entry backed by an external service instead of tenant workloads.
	// Required when deploymentMethod is api; ignored otherwise.
	// +optional
	APIIntegration *APIIntegration `json:"apiIntegration,omitempty"`

	// CompositionRef overrides the Crossplane Composition used to deploy this
	// app. When empty the XRD default (app-default) applies. Set to the name
	// of a profile-scoped composition shipped in gentian-apps/profiles/<name>/composition.yaml
	// (e.g. "app-custom") for profiles that require custom MR graphs in the catalogue repo.
	// +optional
	CompositionRef string `json:"compositionRef,omitempty"`

	// Ingress declares the HTTP routing configuration for this app.
	// When set, the orchestrator creates a Kubernetes Ingress resource and a
	// cert-manager Certificate CR for TLS.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`

	// AdditionalIngresses declares extra Ingress resources to create alongside
	// the primary Ingress. Use this when an app requires a second hostname
	// routed to the same (or a different) Service — for example, an app that
	// requires a separate sandbox subdomain for client-side iframe isolation.
	// Each entry is treated identically to Ingress: the orchestrator creates
	// one Kubernetes Ingress per entry with its own host, TLS secret reference,
	// and lifecycle management (stale entries are deleted on app removal).
	// +optional
	AdditionalIngresses []IngressSpec `json:"additionalIngresses,omitempty"`

	// Sidecars declares companion services deployed by purpose-built compositions
	// alongside the primary app (for example a video sidecar). Sidecar OIDC
	// clients and internal secrets use the synthetic app key {profile}-{sidecar}.
	// +optional
	Sidecars []AppSidecarSpec `json:"sidecars,omitempty"`

	// PostInstallJob declares an optional Kubernetes Job the app-default
	// composition runs alongside this app's Helm release, for app-specific
	// bootstrap that must happen via the app's own admin API (e.g. seeding
	// initial config) rather than Helm values — generic operator/composition
	// behavior available to any profile, not a per-app composition.yaml.
	// Retried automatically the same way every other composed resource in
	// this graph is: the Job is recreated once ttlSecondsAfterFinished
	// expires and the tenant reconciles again, so Script must exit non-zero
	// if a dependency isn't ready yet, and must be idempotent (a no-op once
	// the desired state is already correct). See app-profile-guide.md.
	// +optional
	PostInstallJob *AppPostInstallJob `json:"postInstallJob,omitempty"`

	// PortalTiles defines the tiles this app contributes to the gentian-ui
	// portal when deployed for a tenant in dedicated mode. Each tile creates a
	// portal entry keyed as {tile.name}_{tenantName}.
	// Requires ingress.subDomain to be set (used as the tile base URL).
	// When empty no per-tenant portal entries are created.
	// +optional
	PortalTiles []PortalTileSpec `json:"portalTiles,omitempty"`

	// Provisioning configures how Gentian maps workspace groups to in-app roles.
	// Members of gentian:tenant:<t>:app-admins are reconciled into the privileged
	// role declared here when the app is installed for a tenant.
	// +optional
	Provisioning *ProvisioningSpec `json:"provisioning,omitempty"`

	// Security declares MAC and other security requests for this catalogue entry.
	// Cluster administrators approve waivers via PlatformSecurityPolicy.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`

	// Customization declares which rungs of the Gentian customization ladder this
	// app supports and by which mechanism. Humans and agents read it to select the
	// lowest viable rung for a requested change.
	// See docs/app-customization.md.
	// +optional
	Customization *CustomizationSurface `json:"customization,omitempty"`
}

// ProvisioningSpec declares app-specific user lifecycle mappings.
type ProvisioningSpec struct {
	// PrivilegedRole maps gentian:tenant:<t>:app-admins members to an in-app
	// administrator role (for example the Nextcloud "admin" group).
	// +optional
	PrivilegedRole *PrivilegedRoleSpec `json:"privilegedRole,omitempty"`

	// SyncJob is how this app applies PrivilegedRole. The operator resolves
	// app-admins membership, publishes it, and runs this Job; the script inside
	// speaks whatever protocol the application happens to expose.
	//
	// The division is deliberate and load-bearing: resolving a Keycloak group,
	// detecting membership changes and running a Job to completion are platform
	// concerns, so they live here. Knowing that one app wants a JSON-RPC call
	// and another an OCS POST is not, so it lives in the catalogue entry that
	// owns that app. No application's protocol, endpoint or account model may
	// be encoded in the kernel — see the platform boundary in
	// gentian-apps/docs/app-profile-guide.md.
	// +optional
	SyncJob *ProvisioningJobSpec `json:"syncJob,omitempty"`
}

// ProvisioningJobSpec is an app-supplied Job the operator runs to apply a
// provisioning decision the platform has made.
type ProvisioningJobSpec struct {
	// Image the script runs in. Usually the application's own image, which
	// already has whatever client library its API needs.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Script executed with /bin/sh. It is re-run whenever membership changes,
	// so it must be idempotent: converge the app to exactly the membership it
	// is given rather than applying a delta.
	//
	// The operator provides:
	//   GENTIAN_APP_ADMINS_FILE  path to a JSON array of the privileged
	//                            members, each {"id","username","email"}
	//   GENTIAN_PRIVILEGED_ROLE  spec.provisioning.privilegedRole.name
	//   GENTIAN_TENANT           tenant name
	//   GENTIAN_APP              this profile's name
	// plus every key of envFrom below.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Script string `json:"script"`

	// Env binds individual Secret keys to environment variables. Prefer this
	// over EnvFrom for platform-managed credentials: the keys of an app's
	// sensitive-values Secret are hyphenated (db-host, internal-admin_password),
	// and Kubernetes silently drops any envFrom key that is not a valid
	// identifier — the variable simply never appears and the script fails on
	// something unrelated.
	// +optional
	Env []ProvisioningEnvVar `json:"env,omitempty"`

	// EnvFrom names Secrets in the tenant namespace whose keys become
	// environment variables. Only useful when every key is already a valid
	// environment variable name; see Env above.
	// +optional
	EnvFrom []string `json:"envFrom,omitempty"`

	// ServiceAccountName runs the Job under an existing ServiceAccount.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// ProvisioningEnvVar binds one key of a Secret in the tenant namespace to an
// environment variable in the Job.
type ProvisioningEnvVar struct {
	// Name of the environment variable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Name string `json:"name"`

	// SecretKeyRef is the Secret key to read it from.
	// +kubebuilder:validation:Required
	SecretKeyRef ProvisioningSecretKeyRef `json:"secretKeyRef"`
}

// ProvisioningSecretKeyRef selects one key of one Secret.
type ProvisioningSecretKeyRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// PrivilegedRoleKind is the native app role type referenced by PrivilegedRoleSpec.
// +kubebuilder:validation:Enum=group
type PrivilegedRoleKind string

const (
	// PrivilegedRoleKindGroup maps app-admins to an application-native group.
	PrivilegedRoleKindGroup PrivilegedRoleKind = "group"
)

// PrivilegedRoleSpec identifies the in-app privileged role for app-admins members.
type PrivilegedRoleSpec struct {
	// Kind is the application's native role type. Only "group" is supported today.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=group
	Kind PrivilegedRoleKind `json:"kind"`

	// Name is the in-app role identifier (for example Nextcloud group "admin").
	// It is passed to the sync Job as GENTIAN_PRIVILEGED_ROLE. Apps whose
	// privilege model is a flag rather than a named role may ignore it; set a
	// descriptive value anyway so the declaration reads honestly.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
}

// TileSpec configures a Gentian portal tile icon (52×52 SVG).
// Set icon (catalogue path) or logo (custom data URI). image is source-repo only
// and must be inlined to logo before the AppProfile is applied to a cluster.
type TileSpec struct {
	// Icon selects a pre-made tile from the Gentian catalogue (path 2).
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Icon string `json:"icon,omitempty"`

	// Logo is a custom tile as a data URI (path 1). Mutually exclusive with icon.
	// +optional
	// +kubebuilder:validation:Pattern=`^data:image/svg\+xml;base64,[A-Za-z0-9+/]+=*$`
	Logo string `json:"logo,omitempty"`

	// Image is a profile-relative SVG path used in git only (e.g. assets/tile.svg).
	// Run scripts/sync-profile-tile.py to inline into tile.logo before commit.
	// +optional
	Image string `json:"image,omitempty"`
}

// PortalLinkTarget controls how the portal opens a tile link.
// +kubebuilder:validation:Enum=newwindow;samewindow;embedded
type PortalLinkTarget string

const (
	// PortalLinkTargetNewWindow opens the link in a new browser tab (default).
	PortalLinkTargetNewWindow PortalLinkTarget = "newwindow"
	// PortalLinkTargetSameWindow replaces the current page with the link target.
	PortalLinkTargetSameWindow PortalLinkTarget = "samewindow"
	// PortalLinkTargetEmbedded opens the link inside the gentian-ui window manager.
	PortalLinkTargetEmbedded PortalLinkTarget = "embedded"
)

// PortalTileSpec defines a single tile this app contributes to the portal when
// deployed for a tenant in dedicated mode.
type PortalTileSpec struct {
	// Name is the unique tile identifier, forming the portal entry key:
	// {name}_{tenantName}. Must be unique within an AppProfile. Use kebab-case.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`

	// DisplayName is the localized label shown in the portal.
	// Locale keys use BCP 47-style tags (e.g., "de_DE", "en_US").
	// At least one entry is required.
	// +kubebuilder:validation:Required
	DisplayName map[string]string `json:"displayName"`

	// LinkSuffix is appended to the app's base URL
	// (https://{ingress.subDomain}.{tenantDomain}) to deep-link into a sub-app.
	// Example: "#app=io.ox/mail". Defaults to empty (root URL).
	// +optional
	LinkSuffix string `json:"linkSuffix,omitempty"`

	// LinkTarget controls how the portal opens the tile.
	// newwindow opens in a new browser tab (default).
	// embedded opens inside the gentian-ui window manager (iframe).
	// +optional
	// +kubebuilder:default=newwindow
	// +kubebuilder:validation:Enum=newwindow;samewindow;embedded
	LinkTarget PortalLinkTarget `json:"linkTarget,omitempty"`

	// AllowedGroup is the Keycloak group whose members can see this tile.
	// Defaults to "Domain Users" (all authenticated portal users).
	// Use managed-by-attribute-{Capability} groups (e.g., "managed-by-attribute-Groupware")
	// to restrict to users with a specific provisioned capability.
	// +optional
	// +kubebuilder:default="Domain Users"
	AllowedGroup string `json:"allowedGroup,omitempty"`

	// Tile overrides the app-level tile for this portal entry.
	// +optional
	Tile *TileSpec `json:"tile,omitempty"`
}

// BrowserProxyRoute declares a single proxy route the shell exposes for this app.
type BrowserProxyRoute struct {
	// Path is the route suffix exposed on the shell proxy, e.g. "api"
	// yields /api/apps/{appName}/api. Must not contain leading or
	// trailing slashes. Use kebab-case for multi-segment suffixes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9/-]*[a-z0-9]$|^[a-z0-9]$`
	Path string `json:"path"`

	// Target is the upstream base URL to forward requests to.
	// Supports {name} (app profile name) and {namespace} (tenant
	// namespace) as runtime template variables.
	// Example: "http://{name}.{namespace}.svc/api/v3/"
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Target string `json:"target"`

	// AuthMode controls how the forwarded request is authenticated.
	// forward-bearer: the requesting user's bearer token is forwarded
	// to the upstream (default). none: unauthenticated passthrough
	// for public endpoints.
	// +optional
	// +kubebuilder:validation:Enum=forward-bearer;none
	// +kubebuilder:default=forward-bearer
	AuthMode string `json:"authMode,omitempty"`

	// StripPrefix removes the /api/apps/{appName}/{path} prefix before
	// forwarding to the upstream target. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	StripPrefix bool `json:"stripPrefix"`
}

// IngressSpec declares how the orchestrator should expose this app via HTTP(S).
type IngressSpec struct {
	// ServiceName is the Kubernetes Service name that the Helm chart creates.
	// Defaults to the AppProfile name (chart name) when not set.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// ServicePort is the port on the Service to route traffic to.
	// +optional
	// +kubebuilder:default=80
	ServicePort int32 `json:"servicePort,omitempty"`

	// SubDomain is the subdomain prefix prepended to the tenant domain to form the
	// HTTPRoute host, e.g. "files" yields "files.{tenant-domain}".
	// Defaults to the app profile name (chart name) when not set.
	// +optional
	SubDomain string `json:"subDomain,omitempty"`

	// TLSEnabled enables TLS via a cert-manager Certificate CR.
	// Defaults to true.
	// +optional
	// +kubebuilder:default=true
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// ClusterIssuer is reserved for future per-app issuer overrides. Tenant app
	// ingress TLS is issued by the operator as one DNS-01 wildcard per tenant
	// (*.<effectiveDomain>); see TenantDNS01ClusterIssuer / TENANT_DNS01_CLUSTER_ISSUER.
	// Not used by the ingress reconciler today. Empty is equivalent to
	// "letsencrypt-http01" — left undefaulted at the schema level so profiles
	// that omit it don't permanently diff against GitOps tooling that applies
	// CRD defaults server-side; see docs/design/multi-tenancy.md §3.
	// +optional
	ClusterIssuer string `json:"clusterIssuer,omitempty"`

	// Annotations are merged into Gateway edge policy for this host (frame-ancestors,
	// timeouts, buffer limits). See gentianos.io/gateway-* keys in catalogue_types.go.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// KernelRequirements specifies which kernel services the app requires.
type KernelRequirements struct {
	// Identity specifies OIDC requirements.
	// +optional
	Identity *IdentityRequirement `json:"identity,omitempty"`

	// Database specifies a relational database requirement.
	// +optional
	Database *DatabaseRequirement `json:"database,omitempty"`

	// Storage specifies object storage (S3) and/or filesystem (WebDAV) requirements.
	// +optional
	Storage *StorageRequirement `json:"storage,omitempty"`

	// Cache specifies a caching backend requirement.
	// +optional
	Cache *CacheRequirement `json:"cache,omitempty"`

	// Mail specifies SMTP and/or IMAP requirements.
	// +optional
	Mail *MailRequirement `json:"mail,omitempty"`

	// MCP specifies a Model Context Protocol server requirement.
	// When enabled, the orchestrator registers the app's MCP endpoint in the
	// kernel MCP registry and wires OIDC authentication.
	// +optional
	MCP *MCPRequirement `json:"mcp,omitempty"`
}

// IdentityRequirement specifies OIDC or SAML needs.
type IdentityRequirement struct {
	// OIDC describes the OIDC client to register in the tenant's Keycloak realm.
	// When set, the composition emits a Client CR so every newly deployed tenant
	// automatically gets the correct Keycloak client without manual setup.
	// Leave unset for apps whose Keycloak client is managed externally
	// (e.g. kernel-realm clients managed via keycloak-config).
	// +optional
	OIDC *OIDCClientSpec `json:"oidc,omitempty"`

	// SAML describes the SAML client to register in the tenant's Keycloak realm.
	// +optional
	SAML *SAMLClientSpec `json:"saml,omitempty"`
}

// SAMLClientSpec describes a SAML client to be registered in the tenant's Keycloak realm.
type SAMLClientSpec struct {
	// EntityID is the SAML Service Provider Entity ID registered in Keycloak.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EntityID string `json:"entityId"`

	// ACSURL is the SAML Assertion Consumer Service URL for the auth callback.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ACSURL string `json:"acsUrl"`
}

// OIDCClientSpec describes an OIDC client to be registered in the tenant's
// Keycloak realm by the Crossplane composition. Adding this block to an
// AppProfile is all that is needed to get automatic client registration for
// any new app — no manual Keycloak setup required per tenant.
type OIDCClientSpec struct {
	// ClientID is the OIDC client identifier registered in Keycloak.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientId"`

	// Name is the human-readable display name shown in the Keycloak admin UI.
	// Defaults to ClientID when empty.
	// +optional
	Name string `json:"name,omitempty"`

	// AccessType controls whether the client requires a shared secret
	// (CONFIDENTIAL, for server-side apps) or relies on PKCE without a secret
	// (PUBLIC, for SPAs and mobile apps). Defaults to PUBLIC.
	// +optional
	// +kubebuilder:validation:Enum=PUBLIC;CONFIDENTIAL
	// +kubebuilder:default=PUBLIC
	AccessType string `json:"accessType,omitempty"`

	// RedirectURIs lists the valid OAuth2 redirect URIs for the authorization
	// code flow. Supports ${TENANT_DOMAIN} substitution.
	// +optional
	RedirectURIs []string `json:"redirectUris,omitempty"`

	// PostLogoutRedirectURIs lists the allowed post-logout redirect URIs.
	// Supports ${TENANT_DOMAIN} substitution.
	// +optional
	PostLogoutRedirectURIs []string `json:"postLogoutRedirectUris,omitempty"`

	// BackchannelLogoutURL is the endpoint Keycloak calls for backchannel logout
	// notifications (RFC 7009). Supports ${TENANT_DOMAIN} substitution.
	// +optional
	BackchannelLogoutURL string `json:"backchannelLogoutUrl,omitempty"`

	// DirectAccessGrantsEnabled enables the Resource Owner Password Credentials
	// grant (legacy / CLI apps). Defaults to false.
	// +optional
	DirectAccessGrantsEnabled bool `json:"directAccessGrantsEnabled,omitempty"`

	// OIDCPackRef selects a pack entry from an OIDCPackCatalog CR when the
	// pack key differs from clientId. When empty, clientId is used as the pack key.
	// +optional
	OIDCPackRef string `json:"oidcPackRef,omitempty"`
}

// DatabaseRequirement specifies a relational database need.
type DatabaseRequirement struct {
	// Engine selects the database engine. Defaults to postgresql.
	// +optional
	// +kubebuilder:default=postgresql
	Engine DatabaseEngine `json:"engine,omitempty"`

	// DatabasePerTenant creates a separate database for each tenant (default true).
	// Set to false only for shared-schema apps.
	// +optional
	// +kubebuilder:default=true
	DatabasePerTenant bool `json:"databasePerTenant"`

	// AllowDynamicDatabaseCreation lets the app create databases of its own at
	// runtime — mariadb via GRANT ALL PRIVILEGES ON *.*, postgresql via role
	// CREATEDB. Ask for it only when creating databases is what the app is for
	// (a data explorer, say); an app that merely stores its own state must not.
	//
	// Databases created this way are still purged with the app: they are owned
	// by the app role, and uninstall --purge drops everything that role owns.
	// They are not otherwise governed — the tenant chooses the names, and they
	// do not appear in the catalogue's provisioning model.
	//
	// Omitted is equivalent to false, which is also Go's zero value for bool —
	// left undefaulted at the schema level so profiles that omit it don't
	// permanently diff against GitOps tooling that applies CRD defaults
	// server-side.
	// +optional
	AllowDynamicDatabaseCreation bool `json:"allowDynamicDatabaseCreation,omitempty"`

	// SchemaPreference decides which schema the app's role resolves unqualified
	// table names against first.
	//
	// Default (app-schema) puts the per-tenant schema ahead of public, which is
	// what an app doing its own schema isolation expects. Apps whose migrations
	// create everything in public — and then look it up unqualified — need
	// public first, or the migration writes to one schema and the query reads
	// the other.
	//
	// This is declared rather than inferred: the platform cannot know an app's
	// migration behaviour from its name, and matching on the name is how a
	// second app with the same need silently gets the wrong search_path.
	// +optional
	// +kubebuilder:validation:Enum=app-schema;public
	SchemaPreference SchemaPreference `json:"schemaPreference,omitempty"`
}

// SchemaPreference selects the search_path order for an app's PostgreSQL role.
type SchemaPreference string

const (
	// SchemaPreferenceAppSchema resolves the app's own schema before public.
	// This is the default when unset.
	SchemaPreferenceAppSchema SchemaPreference = "app-schema"
	// SchemaPreferencePublic resolves public before the app's own schema.
	SchemaPreferencePublic SchemaPreference = "public"
)

// StorageRequirement specifies object storage and/or filesystem needs.
type StorageRequirement struct {
	// S3 requests a per-tenant S3 bucket via the MinIO kernel service.
	// +optional
	S3 *S3Requirement `json:"s3,omitempty"`

	// Files requests WebDAV access to the tenant's file-storage service.
	// +optional
	Files *FilesRequirement `json:"files,omitempty"`
}

// S3Requirement describes S3 storage needs.
type S3Requirement struct {
	// BucketPerTenant creates a dedicated bucket for each tenant. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	BucketPerTenant bool `json:"bucketPerTenant"`
}

// FilesRequirement describes WebDAV filesystem access needs.
type FilesRequirement struct {
	// Protocol is the access protocol. Currently only webdav is supported.
	// +kubebuilder:validation:Enum=webdav
	// +kubebuilder:default=webdav
	Protocol string `json:"protocol,omitempty"`

	// Capabilities lists the required access capabilities.
	// +optional
	// +kubebuilder:validation:Items:Enum=read;write
	Capabilities []string `json:"capabilities,omitempty"`
}

// CacheRequirement specifies a caching backend need.
type CacheRequirement struct {
	// Engine selects the caching backend. Defaults to redis.
	// +optional
	// +kubebuilder:default=redis
	Engine CacheEngine `json:"engine,omitempty"`
}

// MailRequirement specifies SMTP and/or IMAP needs.
type MailRequirement struct {
	// SMTP requests outbound mail (SMTP submission) credentials.
	// +optional
	SMTP *SMTPRequirement `json:"smtp,omitempty"`

	// IMAP requests inbound mail (IMAP) credentials.
	// +optional
	IMAP *IMAPRequirement `json:"imap,omitempty"`
}

// SMTPRequirement describes SMTP submission needs.
type SMTPRequirement struct {
	// Auth is the SMTP authentication mechanism.
	// +optional
	// +kubebuilder:validation:Enum=plain;login;cram-md5
	Auth string `json:"auth,omitempty"`

	// Port is the SMTP submission port.
	// +optional
	// +kubebuilder:default=587
	Port int32 `json:"port,omitempty"`
}

// IMAPRequirement describes IMAP access needs.
type IMAPRequirement struct {
	// Port is the IMAP port.
	// +optional
	// +kubebuilder:default=993
	Port int32 `json:"port,omitempty"`
}

// MCPRequirement describes a Model Context Protocol server endpoint.
type MCPRequirement struct {
	// Enabled activates MCP endpoint registration.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// Endpoint is the HTTP path where the app exposes its MCP server.
	// +optional
	// +kubebuilder:default=/mcp
	// +kubebuilder:validation:Pattern=`^/.*`
	Endpoint string `json:"endpoint,omitempty"`

	// Auth is the authentication method for MCP calls.
	// +optional
	// +kubebuilder:validation:Enum=oidc;none
	// +kubebuilder:default=oidc
	Auth string `json:"auth,omitempty"`
}

// ChartRef references an upstream Helm chart.
// APIIntegration configures how an ApiProfile (deploymentMethod: api) reaches
// its backing external service. No workload pods are created; the operator only
// contributes catalogue and portal metadata.
type APIIntegration struct {
	// Runtime selects how the tenant reaches the external service.
	// redirect sends the browser to baseUrl (default).
	// proxy routes through a kernel API proxy (reserved for a later phase).
	// portal-proxy reverse-proxies requests through the portal BFF same-origin.
	// +optional
	// +kubebuilder:default=redirect
	// +kubebuilder:validation:Enum=redirect;proxy;portal-proxy
	Runtime APIIntegrationRuntime `json:"runtime,omitempty"`

	// BaseURL is the external service origin (e.g. https://corp.gentian.org).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://.+`
	BaseURL string `json:"baseUrl"`

	// TenantBinding declares how the tenant is identified to the external
	// service. tenant-domain appends the tenant effective domain as a query
	// parameter (default). none passes no tenant hint.
	// +optional
	// +kubebuilder:default=tenant-domain
	// +kubebuilder:validation:Enum=tenant-domain;none
	TenantBinding APIIntegrationTenantBinding `json:"tenantBinding,omitempty"`
}

// APIIntegrationRuntime selects how an ApiProfile reaches its external service.
// +kubebuilder:validation:Enum=redirect;proxy;portal-proxy
type APIIntegrationRuntime string

const (
	// APIIntegrationRuntimeRedirect sends the browser to the external baseUrl.
	APIIntegrationRuntimeRedirect APIIntegrationRuntime = "redirect"
	// APIIntegrationRuntimeProxy routes via a kernel API proxy (future phase).
	APIIntegrationRuntimeProxy APIIntegrationRuntime = "proxy"
	// APIIntegrationRuntimePortalProxy routes via the portal BFF proxy.
	APIIntegrationRuntimePortalProxy APIIntegrationRuntime = "portal-proxy"
)

// APIIntegrationTenantBinding declares how the tenant is identified to the
// external service.
// +kubebuilder:validation:Enum=tenant-domain;none
type APIIntegrationTenantBinding string

const (
	// APIIntegrationTenantBindingDomain appends the tenant effective domain.
	APIIntegrationTenantBindingDomain APIIntegrationTenantBinding = "tenant-domain"
	// APIIntegrationTenantBindingNone passes no tenant hint.
	APIIntegrationTenantBindingNone APIIntegrationTenantBinding = "none"
)

type ChartRef struct {
	// Repository is the OCI repository URL (e.g., oci://registry.example.com/charts).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Name is the chart name within the repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the chart version to deploy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// ValueMapping declares which Helm value keys receive kernel-provided values.
// The orchestrator validates this schema at admission time and uses it to
// render ExternalSecret targets and Helm values.
type ValueMapping struct {
	// OIDC maps OIDC provider values to Helm keys.
	// +optional
	OIDC *OIDCValueMapping `json:"oidc,omitempty"`

	// Database maps database connection values to Helm keys.
	// +optional
	Database *DatabaseValueMapping `json:"database,omitempty"`

	// S3 maps object storage values to Helm keys.
	// +optional
	S3 *S3ValueMapping `json:"s3,omitempty"`

	// Cache maps caching backend values to Helm keys.
	// +optional
	Cache *CacheValueMapping `json:"cache,omitempty"`

	// SMTP maps mail submission values to Helm keys.
	// +optional
	SMTP *SMTPValueMapping `json:"smtp,omitempty"`

	// IMAP maps mail access values to Helm keys.
	// +optional
	IMAP *IMAPValueMapping `json:"imap,omitempty"`

	// Volumes names where this chart takes extra pod volumes and mounts, for
	// charts that do not use the conventional extraVolumes/extraVolumeMounts.
	// Used when the platform has to mount something into the app itself, such as
	// the staging CA trust bundle.
	// +optional
	Volumes *VolumeValueMapping `json:"volumes,omitempty"`

	// Integrations maps optional integration credentials to Helm keys,
	// keyed by the contract name.
	// +optional
	Integrations map[string]IntegrationValueMapping `json:"integrations,omitempty"`
}

// IntegrationValueMapping maps integration credentials to Helm chart keys.
type IntegrationValueMapping struct {
	// EndpointKey is the Helm value key for the integration's service endpoint URL.
	// +optional
	EndpointKey string `json:"endpointKey,omitempty"`
	// UserKey is the Helm value key for the integration username.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// PasswordKey is the Helm value key for the integration password.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// OIDCValueMapping maps OIDC provider values to Helm chart keys.
// All fields are dot-notation Helm value paths (e.g., "oidc.clientId").
// Deeply nested paths with special characters in keys must use bracket notation
// (e.g., `appsuite.core-mw.secretProperties["com.openexchange.oidc.clientSecret"]`).
type OIDCValueMapping struct {
	// IssuerKey is the Helm value key for the OIDC issuer URL.
	// +optional
	IssuerKey string `json:"issuerKey,omitempty"`
	// ClientIDKey is the Helm value key for the OIDC client ID.
	// +optional
	ClientIDKey string `json:"clientIdKey,omitempty"`
	// ClientSecretKey is the Helm value key for the OIDC client secret.
	// +optional
	ClientSecretKey string `json:"clientSecretKey,omitempty"`
	// ClientSecretOpenbaoPath overrides the default per-tenant OpenBao secret
	// path (gentian-os/tenants/{tenant}/apps/{app}/oidc) for reading the OIDC
	// client secret. Use this for apps that share a kernel-realm OIDC client
	// whose secret is stored at a fixed, non-per-tenant location.
	// Example: "gentian-os/kernel/apps/ox"
	// +optional
	ClientSecretOpenbaoPath string `json:"clientSecretOpenbaoPath,omitempty"`
	// ClientSecretOpenbaoProperty overrides the property name within the
	// OpenBao secret (default: "client-secret"). Only used together with
	// ClientSecretOpenbaoPath.
	// +optional
	ClientSecretOpenbaoProperty string `json:"clientSecretOpenbaoProperty,omitempty"`
}

// DatabaseValueMapping maps database connection values to Helm chart keys.
type DatabaseValueMapping struct {
	// HostKey is the Helm value key for the database host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the database port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// NameKey is the Helm value key for the database name.
	// +optional
	NameKey string `json:"nameKey,omitempty"`
	// ReadNameKey is the Helm value key for the read-replica database name.
	// Useful for charts that expose separate read and write connection endpoints
	// (e.g. OX App Suite's global.mysql.readDatabase).
	// +optional
	ReadNameKey string `json:"readNameKey,omitempty"`
	// UserKey is the Helm value key for the database user.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// ReadUserKey is the Helm value key for the read-replica database user.
	// Useful for charts that expose separate read and write connection endpoints
	// (e.g. OX App Suite's global.mysql.auth.readUser).
	// +optional
	ReadUserKey string `json:"readUserKey,omitempty"`
	// PasswordKey is the Helm value key for the database password.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// S3ValueMapping maps S3 object storage values to Helm chart keys.
type S3ValueMapping struct {
	// EndpointKey is the Helm value key for the S3 endpoint URL.
	// +optional
	EndpointKey string `json:"endpointKey,omitempty"`
	// BucketKey is the Helm value key for the bucket name.
	// +optional
	BucketKey string `json:"bucketKey,omitempty"`
	// AccessKeyKey is the Helm value key for the S3 access key ID.
	// +optional
	AccessKeyKey string `json:"accessKeyKey,omitempty"`
	// SecretKeyKey is the Helm value key for the S3 secret access key.
	// +optional
	SecretKeyKey string `json:"secretKeyKey,omitempty"`
	// RegionKey is the Helm value key for the S3 region.
	// +optional
	RegionKey string `json:"regionKey,omitempty"`
}

// CacheValueMapping maps caching backend connection values to Helm chart keys.
type CacheValueMapping struct {
	// HostKey is the Helm value key for the cache host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the cache port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// UserKey is the Helm value key for the cache ACL username. The kernel
	// provisions a per-app user and records it alongside the password, so profiles
	// should map this key rather than reconstructing the naming rule themselves.
	// Engines without per-app users (memcached) leave it unset.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// PasswordKey is the Helm value key for the cache password/ACL token.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// SMTPValueMapping maps SMTP submission values to Helm chart keys.
type SMTPValueMapping struct {
	// HostKey is the Helm value key for the SMTP host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the SMTP port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
	// UserKey is the Helm value key for the SMTP username.
	// +optional
	UserKey string `json:"userKey,omitempty"`
	// PasswordKey is the Helm value key for the SMTP password.
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// IMAPValueMapping maps IMAP access values to Helm chart keys.
type IMAPValueMapping struct {
	// HostKey is the Helm value key for the IMAP host.
	// +optional
	HostKey string `json:"hostKey,omitempty"`
	// PortKey is the Helm value key for the IMAP port.
	// +optional
	PortKey string `json:"portKey,omitempty"`
}

// AppSidecarSpec declares a companion service deployed alongside the primary
// app by a purpose-built composition (for example a sidecar with its own ingress).
type AppSidecarSpec struct {
	// Name identifies the sidecar (e.g. "sidecar-meet"). OpenBao paths and OIDC jobs
	// use the synthetic app key {parentProfile}-{name} (e.g. "catalogue-test-app-sidecar-meet").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`

	// Chart references the sidecar Helm chart.
	Chart ChartRef `json:"chart"`

	// KernelRequirements declares kernel services the sidecar needs.
	// +optional
	KernelRequirements *KernelRequirements `json:"kernelRequirements,omitempty"`

	// AppSecrets are sidecar-internal secrets stored at
	// gentian-os/tenants/{tenant}/apps/{parent}-{name}/internal/{secret}.
	// +optional
	AppSecrets []AppSecret `json:"appSecrets,omitempty"`

	// ExtraValues are merged into the sidecar Helm release values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`

	// StableServiceName is the Kubernetes Service name the tenant ingress
	// reconciler routes to (e.g. "sidecar-web"). When set, the composition emits
	// a stable ClusterIP alias pointing at the sidecar release.
	// +optional
	StableServiceName string `json:"stableServiceName,omitempty"`

	// StableServicePort is the port on StableServiceName. Consumers (e.g. the
	// odoo-cb-base composition template) already apply their own `default 80`
	// fallback when this is omitted — left undefaulted at the schema level so
	// profiles that omit it don't permanently diff against GitOps tooling that
	// applies CRD defaults server-side.
	// +optional
	StableServicePort int32 `json:"stableServicePort,omitempty"`
}

// AppPostInstallJob configures a Kubernetes Job the app-default composition
// runs in the tenant namespace to bootstrap app-specific state (typically via
// the app's own admin API) that Helm values alone can't express. Kept
// deliberately narrow — not a general PodSpec pass-through — so this stays a
// safe, generic composition feature rather than an arbitrary-workload escape
// hatch. See app-profile-guide.md's "Post-install bootstrap jobs" section.
type AppPostInstallJob struct {
	// Image is the container image the Job runs.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// Script is the shell script body, run via `/bin/sh -c`.
	// +kubebuilder:validation:Required
	Script string `json:"script"`

	// EnvFrom names Secrets already present in the tenant namespace to
	// inject wholesale (matches how this app's own secrets already land —
	// e.g. llm-credentials-{app}, {app}-sensitive-values).
	// +optional
	EnvFrom []string `json:"envFrom,omitempty"`

	// ServiceAccountName runs the Job under an existing ServiceAccount in
	// the tenant namespace (e.g. one already granted an OpenBao Kubernetes-
	// auth role by another part of this app's composition), instead of the
	// namespace default. The composition does not create this account or
	// its RBAC — the profile/composition that needs it is responsible for
	// both, same as any other Kubernetes-native RBAC setup.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// ReadOnlyPVC optionally mounts an existing PersistentVolumeClaim
	// read-only, for the rare case a bootstrap script needs information only
	// available in the app's own persisted data and not obtainable over the
	// network. Requires a storage class that supports concurrent multi-pod
	// mounts even for RWO-labeled claims (e.g. NFS-backed) — most profiles
	// won't need this.
	// +optional
	ReadOnlyPVC *PostInstallJobPVCMount `json:"readOnlyPVC,omitempty"`
}

// PostInstallJobPVCMount is a read-only PVC mount for AppPostInstallJob.
type PostInstallJobPVCMount struct {
	// ClaimName is the PersistentVolumeClaim name in the tenant namespace.
	// +kubebuilder:validation:Required
	ClaimName string `json:"claimName"`

	// MountPath is where the claim is mounted read-only in the Job container.
	// +kubebuilder:validation:Required
	MountPath string `json:"mountPath"`
}

// SidecarAppName returns the synthetic app key used for sidecar OpenBao paths
// and OIDC client jobs ({parent}-{sidecar}).
func SidecarAppName(parentProfile, sidecarName string) string {
	return parentProfile + "-" + sidecarName
}

// AppSecret declares an app-internal secret the orchestrator must generate
// and inject. These are credentials the app needs (admin passwords, session
// signing keys, cluster tokens) that are not provided by any kernel service.
type AppSecret struct {
	// Name is the logical identifier for this secret (e.g., "admin_password").
	// Used as the suffix in the OpenBao path:
	// gentian-os/tenants/{tenant}/apps/{app}/internal/{name}
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9_]+$`
	Name string `json:"name"`

	// ValuePath is the dot-notation (or bracket-notation) Helm value key that
	// should receive the generated secret value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ValuePath string `json:"valuePath"`
}

// DerivedSecretKey names one deterministic value the platform adds to the
// credentials Secret it manages for an app.
//
// The derivation is deliberately frozen: the value is a function of the tenant
// and app name only, so it survives reinstalls. Changing how it is computed
// rotates the value for every existing tenant, which for a session-signing key
// means logging everyone out — so the formula must not be "improved" in place.
// A genuinely better derivation belongs in appSecrets under a new key name.
type DerivedSecretKey struct {
	// Key is the Secret key, and therefore the environment variable name when
	// the app consumes the Secret with envFrom (e.g. "WEBUI_SECRET_KEY").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Key string `json:"key"`
}

// VolumeValueMapping names the values paths a chart uses for extra volumes.
//
// The platform writes extraVolumes/extraVolumeMounts regardless, since that is
// the common convention and an unrecognised value is inert. These paths are
// written *in addition*, so declaring them cannot remove a mount the chart was
// already receiving.
type VolumeValueMapping struct {
	// VolumesPath is the dot-separated values path holding a list of pod volumes
	// (e.g. "volumes").
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_-]*(\.[a-zA-Z0-9][a-zA-Z0-9_-]*)*$`
	VolumesPath string `json:"volumesPath,omitempty"`

	// VolumeMountsPath is the dot-separated values path holding a list of
	// container volume mounts (e.g. "volumeMounts.container").
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_-]*(\.[a-zA-Z0-9][a-zA-Z0-9_-]*)*$`
	VolumeMountsPath string `json:"volumeMountsPath,omitempty"`
}

// ContractRef identifies an integration contract this app provides.
type ContractRef struct {
	// Name is the contract name (e.g., "file-store", "project-management").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`

	// Protocol is the technical protocol used (e.g., "http-json", "webdav").
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// IntegrationRef identifies an optional integration contract this app can consume.
type IntegrationRef struct {
	// Contract is the name of the contract to consume.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Contract string `json:"contract"`

	// Provider is the expected provider app by profile name.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Capabilities lists the specific capabilities required from the contract.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`
}

// AppProfile is the Schema for the appprofiles API.
//
// AppProfile is cluster-scoped — one per app type, shared across all tenants.
// An AppProfile wraps an upstream Helm chart with the schema-based value
// mapping and kernel requirements that the orchestrator needs to provision
// and wire the app for any tenant.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ap;aps
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.family`
// +kubebuilder:printcolumn:name="Cat.Version",type=string,JSONPath=`.spec.catalogueVersion`
// +kubebuilder:printcolumn:name="Edition",type=string,JSONPath=`.spec.edition`
// +kubebuilder:printcolumn:name="Trust",type=string,JSONPath=`.spec.trustTier`
// +kubebuilder:printcolumn:name="License",type=string,JSONPath=`.spec.license`
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.chart.name`
// +kubebuilder:printcolumn:name="Chart Ver",type=string,JSONPath=`.spec.chart.version`
// +kubebuilder:printcolumn:name="Method",type=string,JSONPath=`.spec.deploymentMethod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AppProfileSpec `json:"spec,omitempty"`
}

// AppProfileList contains a list of AppProfile.
// +kubebuilder:object:root=true
type AppProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppProfile{}, &AppProfileList{})
}
