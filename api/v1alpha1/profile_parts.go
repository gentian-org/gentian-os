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

// The parts a ComponentProfile is made of: what it requires of the platform,
// what it is packaged as, how its values are mapped, what it backs up, and
// what it puts on a desktop.
//
// This was appprofile_types.go. AD-4 says there is one catalogue kind, and
// AppProfile was the other one -- so the kind is gone, along with the portal
// tile and the browser-proxy route that only it had. What is left is what
// ComponentProfile was always built from and had merely inherited from a file
// named after something else.
//
// Separate from componentprofile_types.go because the two together are two
// thousand lines, and the split that reads best is the kind in one file and
// its parts in another.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

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

// TileSpec configures a desktop tile icon (52×52 SVG).
// Set icon (catalogue path) or logo (custom data URI). image is source-repo only
// and must be inlined to logo before the profile is applied to a cluster.
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
	// Run gentian-apps' scripts/sync-profile-tile.py to inline into tile.logo
	// before commit — profiles live in that repository, and so does the script.
	// +optional
	Image string `json:"image,omitempty"`
}

// ServiceRequirements is what the platform must fulfil before a component
// runs: identity, a database, object storage, a cache, mail, MCP.
//
// It was called KernelRequirements, which named a fulfiller that is not the
// fulfiller. A database comes from a component of class "service"; mail may
// be a relay outside the cluster; object storage may be a bucket at a cloud
// provider. ComponentProfile spells it requires.services; an extension
// (spec.extensions[]) still writes the key kernelRequirements.
type ServiceRequirements struct {
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

	// MCP declares that the component exposes a Model Context Protocol
	// endpoint. It is a declaration only: there is no MCP registry, and the
	// platform registers nothing and wires no authentication for it. Its one
	// effect today is that a chart component declaring it is installed
	// through the app Composition.
	// +optional
	MCP *MCPRequirement `json:"mcp,omitempty"`

	// LLM declares that the component calls language models through the
	// platform's model gateway. Only a component that declares it is given a
	// key at the gateway, the address and the key, and a network path to the
	// gateway; a component that does not is given none of the three.
	// +optional
	LLM *LLMRequirement `json:"llm,omitempty"`

	// Rights declares that the component asks the platform whether a person
	// may use an app of its tenant, for a person who is not at a browser.
	// Only a component that declares it is given a key for that question and
	// a network path to where it is answered.
	// +optional
	Rights *RightsRequirement `json:"rights,omitempty"`

	// Vouching declares that the component obtains tokens of a person who is
	// not at a browser, on the strength of that person's earlier consent: it
	// signs a short statement naming the person, and the tenant's realm
	// answers with a token of that person for one app.
	// +optional
	Vouching *VouchingRequirement `json:"vouching,omitempty"`
}

// VouchingRequirement declares that a component vouches for people at the
// tenant's realm (RFC 7523, the JWT authorization grant).
//
// The platform then, for each install:
//
//   - enters the component in the tenant's realm as a trusted issuer, under
//     the alias vouch-<profile>, with the address its keys are published at;
//   - gives it a client of the realm to ask with, vouch-<profile>, which can
//     do nothing else, and which may ask for the scope of any app of the
//     tenant that has one (app-<profile>, which makes that app the token's
//     audience);
//   - delivers the client's secret and what the component has to know in the
//     Secret vouching-<profile> of the tenant's namespace (VOUCHING_ISSUER,
//     VOUCHING_CLIENT_ID, VOUCHING_CLIENT_SECRET, VOUCHING_TOKEN_URL);
//   - gives it the component's own key (the Secret rights-check-<profile>),
//     as for requires.services.rights.
//
// The realm issues a token only for a person who is linked to that issuer.
// Nothing here links anybody: a person is linked on a request that carries
// that person's own token, and unlinked on theirs, an administrator's, or
// the component's own. So the people a component can speak for are the
// people who said so.
//
// A component that can obtain a person's token for any app of the tenant is
// of platform trust.
type VouchingRequirement struct {
	// Keys says where the component publishes the keys its statements are
	// signed with, as a JWKS. The realm fetches them from there.
	Keys VouchingKeys `json:"keys"`
}

// VouchingKeys is the address of a component's signing keys: a Service of the
// component, in the tenant's namespace.
type VouchingKeys struct {
	// Service is the name of the component's Service.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Service string `json:"service"`

	// Port is the Service's port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Path is where the JWKS is served.
	// +optional
	// +kubebuilder:default="/.well-known/jwks.json"
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9._~/-]*$`
	// +kubebuilder:validation:MaxLength=256
	Path string `json:"path,omitempty"`
}

// RightsRequirement declares that a component asks the platform one question:
// may this person use that app of the component's own tenant. Being present
// is the declaration.
//
// It is for a component that acts for a person who has no session at the
// edge at that moment, and which would otherwise need the authorization
// store's own credential. The component receives the address the question is
// answered at and a key of its own -- random, per tenant and component -- in
// the Secret rights-check-<component> of the tenant's namespace
// (RIGHTS_CHECK_URL, RIGHTS_CHECK_KEY). The key is good for that question
// about that tenant, and for nothing else: it cannot write, cannot list and
// cannot name another tenant's app.
//
// The answer says who in the tenant may use what, so the requirement is for
// a profile of platform trust.
type RightsRequirement struct{}

// LLMRequirement declares that a component uses the platform's model gateway.
// Being present is the declaration.
//
// The gateway speaks the OpenAI API. The component receives its base address
// and a key of its own -- random, per tenant and component, never derived
// from their names -- in the Secret llm-credentials-<component> of the
// tenant's namespace (OPENAI_API_BASE, OPENAI_API_BASE_URL, OPENAI_API_KEY),
// and, where its chart takes them as values, through valueMapping.llm.
//
// It is served for an app a tenant installs, and for a component the
// platform places on tenants itself (defaultForTenants, defaultForPlatform,
// defaultWhereStoreOffered) when its profile is of platform trust.
//
// On a cluster that runs no model gateway the requirement cannot be met, and
// the component waits and says so, as it does for a database that is not
// there -- unless the requirement is optional.
type LLMRequirement struct {
	// Optional means the component runs without the gateway. While the
	// cluster serves no models, or the component's key is not delivered yet,
	// it is released all the same: without credentials, without a network
	// path to the gateway, and told so through valueMapping.llm.availableKey.
	// When the key arrives the component is rendered again with it.
	//
	// Honoured for a component the platform places on tenants itself. For an
	// app a tenant installs the requirement holds the release as if this
	// were not set: its chart is rendered by the app Composition, which
	// reads the key from where it is kept and cannot render without it.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// IdentityRequirement says how the app signs people in: with an OIDC client
// of its own, with a SAML client of its own, or -- for an app that can do
// neither -- through the platform's sign-in sidecar.
//
// +kubebuilder:validation:XValidation:rule="!has(self.sidecar) || (!has(self.oidc) && !has(self.saml))",message="identity.sidecar signs people in for an app that can do neither OIDC nor SAML itself: it cannot be declared beside identity.oidc or identity.saml"
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

	// Sidecar signs people in through the platform's sign-in sidecar.
	// +optional
	Sidecar *SignInSidecarSpec `json:"sidecar,omitempty"`
}

// SignInSidecarSpec declares that an app's people are signed in by the
// platform's sign-in sidecar: for an app that, in the edition installed,
// speaks neither OIDC nor SAML, so that a person who is signed in at the
// platform opens it and is in -- with no second sign-in and no password.
//
// Declaring it is all a profile does. The platform then, for each install:
//
//   - registers the sidecar as a SAML service provider in the tenant's realm,
//     under a name and with the one address it derives from where the app
//     answers. A profile states neither;
//   - runs the sidecar beside the app, told where the realm is, and gives it
//     the handler from the profile's bundle;
//   - routes two paths of the app's own host to it: /sso/login, behind the
//     front door like the rest of the app, and /sso/acs, which the realm
//     posts its answer to and which therefore takes no session;
//   - opens the sidecar's network paths: to the realm's certificate, and to
//     what is declared below, and to nothing else;
//   - removes all of it with the app.
//
// The handler is the app's part: the code that makes a session in this app
// for the person the sidecar names. It is the ConfigMap
// "<profile>.sign-in-handler" of the profile's bundle (key handler.js, label
// gentianos.io/asset: sign-in-handler), so the install's digest covers it. A
// handler holds whatever is declared here, which is why it runs only from a
// bundle the install is pinned to, and never from a tenant's own catalogue.
//
// Who may use the app is not decided here. A sign-in begins on /sso/login,
// which only a person the front door admits to this app reaches.
type SignInSidecarSpec struct {
	// Exposure names the gateway entry people open the app at: the sidecar's
	// two paths are on that entry's host. It may be omitted when the profile
	// has exactly one gateway entry with authMode oidc.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Exposure string `json:"exposure,omitempty"`

	// EntryPaths are paths of the app that lead to the sign-in instead of to
	// the app: its front page, and the address of its own sign-in form.
	// Matched exactly, and only for a page load; a request the app's own page
	// makes in the background is the app's to deal with. The page a handler
	// sends a person to after signing them in must not be among them, and
	// neither may a path under /sso/ or /oauth2/, which are the sign-in's own:
	// the operator holds an install whose profile names one.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^/[A-Za-z0-9._~/-]*$`
	EntryPaths []string `json:"entryPaths,omitempty"`

	// Database gives the handler the app's own database: where it is and the
	// app's own login to it, as DB_HOST, DB_PORT, DB_NAME, DB_USER and
	// DB_PASSWORD, and the network path to that server. The profile must
	// declare requires.services.database.
	// +optional
	Database bool `json:"database,omitempty"`

	// Secrets gives the handler secrets of this app, by the names they have
	// under spec.secrets.generated: each as SECRET_<NAME>, the name in upper
	// case. An app's session-signing key is the usual one. A name that is not
	// under spec.secrets.generated holds the install: what is read is always
	// this app's own vault path, so no name reaches another app's secret.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9_]+$`
	Secrets []string `json:"secrets,omitempty"`

	// AppPort lets the handler call the app itself inside the cluster: it
	// is the port the app's pods listen on, and the network path is opened
	// to that port of the app's own pods. The handler is told where the app
	// is as APP_URL -- the Service and port the exposure routes to, which
	// need not carry the same number. Such a call does not pass the front
	// door.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	AppPort int32 `json:"appPort,omitempty"`
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
// profile is all that is needed to get automatic client registration for
// any new app — no manual Keycloak setup required per tenant.
//
// +kubebuilder:validation:XValidation:rule="!has(self.backchannelLogoutUrl) || self.backchannelLogoutUrl.size() == 0",message="backchannelLogoutUrl is withdrawn: declare backchannelLogout (exposure and path) instead. The platform builds the address from the entry's own Service inside the cluster; a profile no longer names one"
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

	// BackchannelLogoutURL is withdrawn and refused: declare BackchannelLogout
	// instead.
	//
	// It was an address of the profile's own writing, which the realm then
	// posted a logout token to when a person signed out. Two things were
	// wrong with that. Every profile wrote the app's public address, where
	// every path is behind a session, so the realm's own request was answered
	// with a redirect to the sign-in and never arrived. And it let a
	// catalogue entry make the identity provider send a request to any
	// address at all, inside the cluster or outside it.
	//
	// The field stays in the definition so that a profile which still
	// carries it is refused and told why, where an unknown field would be
	// dropped without a word.
	// +optional
	BackchannelLogoutURL string `json:"backchannelLogoutUrl,omitempty"`

	// BackchannelLogout says where in the app the realm tells it that a
	// person signed out (OpenID Connect Back-Channel Logout).
	// +optional
	BackchannelLogout *BackchannelLogoutSpec `json:"backchannelLogout,omitempty"`

	// DirectAccessGrantsEnabled enables the Resource Owner Password Credentials
	// grant (legacy / CLI apps). Defaults to false.
	// +optional
	DirectAccessGrantsEnabled bool `json:"directAccessGrantsEnabled,omitempty"`

	// OIDCPackRef selects a pack entry from an OIDCPackCatalog CR when the
	// pack key differs from clientId. When empty, clientId is used as the pack key.
	// +optional
	OIDCPackRef string `json:"oidcPackRef,omitempty"`
}

// BackchannelLogoutSpec declares where an app with its own OIDC client is
// told that a person signed out.
//
// When a person signs out at the platform, the realm ends its session and
// posts a logout token -- a JWT it signs, naming the client, the person and
// the session -- to every client of that session which registered an address
// for it. The app checks the token and ends its own session for that person,
// which otherwise outlives the sign-out: the next person at the same browser
// would open the app as the one before.
//
// A profile states a path and which of its entries the path belongs to. It
// states no address. The platform builds one from the entry's own backend:
//
//	http://<backend.service>.<the component's namespace>.svc.cluster.local:<backend.port><path>
//
// So the realm calls the app inside the cluster, server to server, at the
// app's own Service. It does not call the app's public address, where the
// front door would ask the realm for a session it does not have; and it can
// be made to call nothing but the Service this component's own entry routes
// to -- no other namespace, no address outside the cluster.
//
// Nothing is opened for it. The realm's namespace is already admitted to a
// tenant's pods, and the path is no more public than before.
//
// What the app has to do is its own, and the profile's author checks it: the
// app must verify the token's signature against the realm's keys, its issuer,
// that its audience is this client, and that it carries the back-channel
// logout event, before it ends a session. An app that ends a session on an
// unverified token lets whoever can reach that path sign people out -- never
// in -- so say in the profile's customization record what the app checks.
//
// The realm posts once, when a person signs out or an administrator ends
// their session. It does not post again if the app did not answer, and not
// when a session merely runs out.
type BackchannelLogoutSpec struct {
	// Exposure names the entry of this profile (spec.expose) whose backend
	// the path is served by. The entry must route to this component's own
	// Service: one that names another component's (backend.component) is
	// refused, and so is a name the profile has no entry for.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Exposure string `json:"exposure"`

	// Path is the app's back-channel logout endpoint: a plain path. One or
	// more segments of letters, digits, "_", "~" and "-", which may have
	// single dots inside them. So no query and no fragment, no empty segment
	// ("//", which begins a host), no ".." and no segment that begins or ends
	// with a dot, and nothing else that could be read as part of an address.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^(/[A-Za-z0-9_~-]+(\.[A-Za-z0-9_~-]+)*)+/?$`
	Path string `json:"path"`
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
	// runtime. Ask for it only when creating databases is what the app is for
	// (a data explorer, say); an app that merely stores its own state must not.
	//
	// On postgresql the app's role is given CREATEDB. Databases created this
	// way are the app's by ownership: the app role owns each one it made, so
	// an export copies them, a restore puts them back and a purge drops them
	// with the role. They are not otherwise governed — the tenant chooses the
	// names, and they do not appear in the catalogue's provisioning model.
	//
	// On mariadb a database has no owner and the server is shared by every
	// tenant, so the app's databases are the ones named for it: the app may
	// create, use and drop databases whose name is the name of its
	// provisioned database followed by an underscore and anything —
	// "<database>_reports" — and no other. It is granted all privileges on
	// those names and on its provisioned database, and none on the server:
	// CREATE DATABASE under any other name is refused, as is any statement
	// that needs a global privilege. An app that names its databases itself
	// must therefore be configured, by the profile, to prefix them with
	// "<database>_"; one that cannot be is not installable with this field on
	// the shared server. Databases so named are exported, restored (under the
	// provisioned name of the tenant restored into) and purged with the
	// provisioned one. Provisioning refuses an app whose provisioned database
	// would itself lie under another app's prefix, or the reverse.
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

// SMTPRequirement declares that an app sends mail. It carries nothing.
//
// It had auth and port, and neither was ever read — not by the operator, not by
// any Composition. They also asked the wrong party: the mechanism a server
// accepts and the port it listens on are the platform's to know, and an app
// asserting "587, plain" is asserting something it cannot verify and would be
// wrong about the moment the cluster changed. The values an app receives are
// mapped through valueMapping.smtp, which is where the app describes its own
// chart rather than the cluster's mail server.
//
// Empty on purpose, and a struct rather than a bool so a future field that is
// genuinely the app's to state — a required TLS level, say — has somewhere to
// go without changing every profile.
type SMTPRequirement struct{}

// IMAPRequirement declares that an app reads mail. The mail server's address
// is the cluster's, for the same reason as in SMTPRequirement.
type IMAPRequirement struct {
	// TokenSignIn declares that the app opens a person's mailbox with that
	// person's sign-in token (SASL XOAUTH2) instead of an app password.
	//
	// It is a grant, which is why it is said and never implied by imap: the
	// mail server accepts a token only from the sign-in client of an app
	// whose profile declares this. The app's client is given the optional
	// client scope "mailbox" in the tenant's realm; a token the app obtained
	// by asking for that scope names the mail server in its audience and is
	// accepted for the mailbox of the person it was issued to, and no other
	// token of the app, and no token of any other app, is.
	//
	// It needs requires.services.identity.oidc, the client the scope is given
	// to. Served where the cluster runs its own mail server and the tenant
	// has mailboxes on it in a realm of its own; elsewhere it does nothing.
	// +optional
	TokenSignIn bool `json:"tokenSignIn,omitempty"`
}

// MCPRequirement describes a Model Context Protocol server endpoint. Nothing
// reads its fields yet (see ServiceRequirements.MCP).
type MCPRequirement struct {
	// Enabled says the component exposes the endpoint.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// Endpoint is the HTTP path where the app exposes its MCP server.
	// +optional
	// +kubebuilder:default=/mcp
	// +kubebuilder:validation:Pattern=`^/.*`
	Endpoint string `json:"endpoint,omitempty"`

	// Auth is the authentication method the endpoint expects.
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

	// BaseURL is the external service origin (e.g. https://service.example).
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

	// Tile puts this entry on the desktop of whoever holds its relation.
	//
	// An entry that runs nothing has nothing to expose, so it cannot carry a
	// tile the way a component does, under an exposure with a backend. The
	// tile is here instead and leads straight to baseUrl: there is no host
	// of the cluster's in between, because a redirect through one would be a
	// route, a certificate and a session for the sake of a link.
	//
	// Read for runtime redirect only. The proxy runtimes put a host of the
	// cluster's in front of the service, and that host is what a tile of
	// theirs would name.
	// +optional
	Tile *ExposureTile `json:"tile,omitempty"`
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

	// LLM maps the model gateway's address and the component's key to Helm
	// keys, for a component that declares requires.services.llm.
	// +optional
	LLM *LLMValueMapping `json:"llm,omitempty"`

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

	// Platform maps the facts of the cluster a component runs in to Helm keys:
	// which zone it is served in, which realm signs that zone's tokens, where
	// the director answers. These are not requirements the platform fulfils
	// on the component's behalf, like a database; they are things every
	// component behind the edge has to be told and none can discover.
	//
	// Declared per profile rather than handed to one profile by name. The
	// desktop used to receive exactly these values through a special case
	// keyed on its annotation, which meant no second component could be built
	// from the app template and put behind the edge without another special
	// case. A profile that names no key here receives nothing, which is what
	// a component with no exposure wants.
	// +optional
	Platform *PlatformValueMapping `json:"platform,omitempty"`
}

// PlatformValueMapping names where a chart takes the facts of the cluster it
// runs in. Every field is a dot-notation Helm value path; an empty field is
// simply not set. The values themselves are the operator's to know: nothing
// here is configurable, only where it lands.
type PlatformValueMapping struct {
	// IssuerKey receives the zone's OIDC issuer, the realm on the identity
	// provider whose tokens the edge forwards. A backend verifies the
	// forwarded token against it and nothing else.
	// +optional
	IssuerKey string `json:"issuerKey,omitempty"`
	// ZoneClientIDKey receives the client id the edge holds the zone's session
	// with. Not the component's own client, which comes with the identity
	// requirement; this is the one whose token arrives on forwardToken routes.
	// +optional
	ZoneClientIDKey string `json:"zoneClientIdKey,omitempty"`
	// AudienceKey receives the audience the forwarded edge token carries, so
	// a backend can require it rather than accept any token the realm signs.
	// +optional
	AudienceKey string `json:"audienceKey,omitempty"`
	// DirectorURLKey receives the director's in-cluster URL. Only meaningful
	// on a platform-trust component that relays the forwarded token there;
	// naming it also opens the component's egress to the control namespace.
	// +optional
	DirectorURLKey string `json:"directorUrlKey,omitempty"`

	// UsherURLKey receives the usher's in-cluster URL: where a desktop asks,
	// with the person's own token, what that person may open. Naming it also
	// opens the component's egress to the control namespace, where the usher
	// runs.
	// +optional
	UsherURLKey string `json:"usherUrlKey,omitempty"`

	// CustodianURLKey receives the custodian's in-cluster
	// URL. Like DirectorURLKey it is more than a fact: naming it is what
	// opens the component's egress to the control namespace, because a
	// component that does not relay there has no business reaching it.
	//
	// The custodian holds no authority of its own -- every write
	// takes the caller's token and exchanges it for one OpenBao will accept
	// -- so a component that relays to it is passing the person through,
	// exactly as it does with the director.
	// +optional
	CustodianURLKey string `json:"custodianUrlKey,omitempty"`

	// RegistrarURLKey receives the registrar's in-cluster URL: where a
	// console relays, with the person's own token, the managing of a
	// tenant's people and groups. Like the two above, naming it is what
	// opens the component's egress to the control namespace.
	//
	// The registrar serves the paths the director served for people, so a
	// component that called the director for them changes the address and
	// nothing else.
	// +optional
	RegistrarURLKey string `json:"registrarUrlKey,omitempty"`
	// ClusterKey receives the id the director knows this cluster by.
	// +optional
	ClusterKey string `json:"clusterKey,omitempty"`
	// TenantKey receives the name of the tenant the component runs in.
	// +optional
	TenantKey string `json:"tenantKey,omitempty"`
	// KernelDomainKey receives the cluster's kernel domain.
	// +optional
	KernelDomainKey string `json:"kernelDomainKey,omitempty"`
	// RealmKey receives the name of the zone's realm.
	// +optional
	RealmKey string `json:"realmKey,omitempty"`
	// ZoneKindKey receives "kernel" for a component served in the kernel zone
	// and "tenant" otherwise. A component whose behaviour differs between the
	// platform's own zone and a customer's decides that from this, rather
	// than from a list of capabilities the operator composes for it.
	// +optional
	ZoneKindKey string `json:"zoneKindKey,omitempty"`

	// DefaultLanguageKey receives the tenant's own language, as an ISO 639-1
	// code (AD-15). It is what a person sees before they have chosen one and
	// before any settings template has given them one — a German tenant's
	// people get a German desktop on their first sign-in rather than whatever
	// their browser happens to ask for.
	//
	// The tenant's language is the FIRST of the languages it declares, because
	// the order is the preference: a tenant listing de then en is saying it is
	// German-speaking and also serves English.
	// +optional
	DefaultLanguageKey string `json:"defaultLanguageKey,omitempty"`

	// HostKey receives the host this component answers on in its zone: the
	// host of its first gateway entry, as the operator routes it. A component
	// that has to spell its own address -- a redirect address it states to a
	// service outside the cluster -- is told it rather than left to assemble
	// it from the tenant and the domain, which differs by tenancy mode.
	// +optional
	HostKey string `json:"hostKey,omitempty"`

	// StoreURLKey receives the base address of the App Store API this cluster
	// names (the Cluster claim's spec.catalogue.storeUrl), and the empty
	// string while the cluster offers no App Store: none is named, or licence
	// reporting is off.
	//
	// Like the URL keys above it is more than a fact. A store is outside the
	// cluster, and so are the addresses its own metadata names, so naming this
	// key is what opens the component's pods a way out: TCP 443 to public
	// addresses, and only while a store is offered. That is why only a
	// platform-trust profile may name it.
	// +optional
	StoreURLKey string `json:"storeUrlKey,omitempty"`
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
	// SecretNameKey receives the NAME of the Secret the platform wrote the
	// database credentials into, as a plain string, for a chart that consumes
	// it with envFrom or a secretKeyRef rather than taking each value apart.
	// This is the Pattern A shape the desktop and the app template use:
	// credentials are never chart values, and the chart is told only where
	// they are. HostKey below receives a structured reference instead, for
	// charts that take a host with a valueFrom; the two are not the same and
	// a chart wants one or the other.
	// +optional
	SecretNameKey string `json:"secretNameKey,omitempty"`
	HostKey       string `json:"hostKey,omitempty"`
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

// LLMValueMapping maps the model gateway's values to Helm chart keys.
type LLMValueMapping struct {
	// BaseURLKey is the Helm value key for the gateway's OpenAI-compatible
	// base address (it ends in /v1).
	// +optional
	BaseURLKey string `json:"baseUrlKey,omitempty"`
	// APIKeyKey is the Helm value key for the key the component presents.
	// For an app a tenant installs, where the app Composition sets it from
	// the vault. A component the platform places on tenants itself is
	// rendered by the operator, whose release values are not a place for a
	// key: such a profile names SecretNameKey instead, and is refused if it
	// names this.
	// +optional
	APIKeyKey string `json:"apiKeyKey,omitempty"`
	// SecretNameKey receives the NAME of the Secret the component's model
	// credentials are in (llm-credentials-<component>, in the component's
	// namespace; the key is under OPENAI_API_KEY), as a plain string, for a
	// chart that reads the key with a secretKeyRef or envFrom. The same shape
	// as database.secretNameKey: the key is never a chart value. Empty while
	// an optional requirement is not met. For a component the platform
	// places on tenants itself.
	// +optional
	SecretNameKey string `json:"secretNameKey,omitempty"`
	// AvailableKey receives true when the gateway's address and the
	// component's key are delivered, and false while an optional requirement
	// (requires.services.llm.optional) is not met. For a component the
	// platform places on tenants itself.
	// +optional
	AvailableKey string `json:"availableKey,omitempty"`
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

	// ServiceRequirements declares kernel services the sidecar needs.
	// +optional
	ServiceRequirements *ServiceRequirements `json:"kernelRequirements,omitempty"`

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

// BackupSpec tells the platform how this app must be captured and put back.
// spec.requires.services already declares *which* stores the app has; this
// declares how to pause it, what on its volumes is worth keeping, and what has
// to run after a restore before the app is usable again — knowledge only the
// app's author has.
//
// Every field is optional, and a profile that declares nothing gets the safe
// default: scale the app to zero, dump every store in spec.requires.services,
// and archive every PersistentVolumeClaim its Helm release owns. That is
// correct for most apps, so the section exists for the ones that deviate.
//
// Schedule, retention, encryption and destination are deliberately absent: they
// are platform and tenant policy, and an app author must not be able to weaken
// them.
type BackupSpec struct {
	// Quiesce controls how writes are paused while the app is captured.
	// +optional
	Quiesce *BackupQuiesce `json:"quiesce,omitempty"`

	// Volumes narrows what is captured from the app's PersistentVolumeClaims.
	// +optional
	Volumes *BackupVolumes `json:"volumes,omitempty"`

	// BoundSecrets lists secrets this app's data is welded to: not derived, held
	// outside the app's own captured data, and required to make sense of that
	// data again. They travel inside the encrypted bundle.
	//
	// Most apps need none. Values under spec.appSecrets are HMAC-derived from
	// the master password and reproduce byte-identically, and a secret an app
	// writes into its own config file on a captured volume already travels with
	// that volume.
	// +optional
	BoundSecrets []BackupBoundSecret `json:"boundSecrets,omitempty"`

	// Restore declares what must run after this app's data is loaded and before
	// it is resumed.
	// +optional
	Restore *BackupRestore `json:"restore,omitempty"`

	// Consistency selects whether all of this app's stores must be captured
	// inside one quiesce window. Defaults to app.
	// +optional
	// +kubebuilder:default=app
	Consistency BackupConsistency `json:"consistency,omitempty"`

	// MinRestoreVersion refuses to restore a bundle into an app older than this
	// version, expressed as the app version the profile's chart deploys.
	// Restoring older data into a newer app is allowed — that just runs the
	// app's own migrations — but the reverse silently corrupts.
	// +optional
	MinRestoreVersion string `json:"minRestoreVersion,omitempty"`
}

// BackupQuiesce declares how to pause an app's writes for the duration of a
// capture. Pausing is the only way to get a consistent view across an app's
// database, buckets and volumes: nothing else coordinates independent stores.
// +kubebuilder:validation:XValidation:rule="self.mode != 'command' || (has(self.pre) && size(self.pre) > 0)",message="quiesce.pre is required when mode is command"
type BackupQuiesce struct {
	// Mode selects how writes are paused. Defaults to scaleDown, which works
	// for every app; command is better where the app has a real maintenance
	// mode, because it keeps the app reachable while it is captured.
	// +optional
	// +kubebuilder:default=scaleDown
	Mode BackupQuiesceMode `json:"mode,omitempty"`

	// Pre is the command that pauses writes, required when mode is command.
	// This is an argv, not a shell line: the first element is the binary and
	// the rest are its arguments, so nothing is word-split or glob-expanded.
	// +optional
	Pre []string `json:"pre,omitempty"`

	// Post is the command that resumes writes. It must be safe to run when Pre
	// never ran or already resumed — it also runs on the failure path, and an
	// app left paused is an outage.
	// +optional
	Post []string `json:"post,omitempty"`

	// Container names the container to run Pre and Post in. Defaults to the
	// first container in the app's pod.
	// +optional
	Container string `json:"container,omitempty"`
}

// BackupVolumes narrows what is captured from an app's volumes.
type BackupVolumes struct {
	// Include names the PersistentVolumeClaims to capture. When empty, every
	// claim the app's Helm release owns is captured.
	// +optional
	Include []string `json:"include,omitempty"`

	// ExcludePaths drops matching paths from the captured archive, as glob
	// patterns relative to each volume root. Use it for data the app rebuilds
	// by itself — thumbnails, search indexes, caches — which is often most of
	// an app's disk.
	//
	// Never exclude a path holding app configuration. An app that keeps the key
	// its data was encrypted with in its own config file becomes unrestorable
	// the moment that file is excluded, and nothing detects it until a restore.
	// +optional
	ExcludePaths []string `json:"excludePaths,omitempty"`
}

// BackupBoundSecret references a secret in the tenant's OpenBao tree that must
// travel with the app's data.
type BackupBoundSecret struct {
	// OpenBaoPath is relative to this tenant's own prefix
	// (gentian-os/tenants/{tenant}/), so a profile can only ever name secrets
	// belonging to the tenant being captured.
	//
	// The pattern enforces that containment: it accepts slash-separated segments
	// that each begin with an alphanumeric, which rejects a leading slash and
	// any ".." segment — the only forms that escape the prefix. Expressing it as
	// a pattern rather than a CEL rule is deliberate. CEL here costs more than
	// it is worth: Kubernetes bounds the estimated cost of every rule in a CRD,
	// and one rule over an unbounded string inside an unbounded list put the
	// whole AppProfile schema over budget, so the API server refused to install
	// it at all. The pattern is also the more precise statement, since a name
	// like "my..app" traverses nothing and should be allowed.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*(/[a-zA-Z0-9][a-zA-Z0-9._-]*)*$`
	OpenBaoPath string `json:"openBaoPath"`

	// Keys selects individual keys at that path. When empty, every key there is
	// captured.
	// +optional
	Keys []string `json:"keys,omitempty"`
}

// BackupRestore declares what runs after an app's data is put back.
type BackupRestore struct {
	// Post lists commands run in order once the app's data is loaded and before
	// it is resumed — cache invalidation, re-indexing, fingerprinting. Each
	// entry is an argv, for the same reason as BackupQuiesce.Pre.
	// +optional
	Post [][]string `json:"post,omitempty"`

	// Verify is a single command that must exit zero before the restore is
	// reported successful. Without one, a restore can only report that the data
	// was written, not that the app can read it.
	// +optional
	Verify []string `json:"verify,omitempty"`
}

// QuiesceMode returns how this app's writes should be paused, resolving the
// default for a profile that declares no backup block.
func (b *BackupSpec) QuiesceMode() BackupQuiesceMode {
	if b == nil || b.Quiesce == nil || b.Quiesce.Mode == "" {
		return BackupQuiesceScaleDown
	}
	return b.Quiesce.Mode
}

// ConsistencyMode returns the window this app's stores must be captured within,
// resolving the default.
func (b *BackupSpec) ConsistencyMode() BackupConsistency {
	if b == nil || b.Consistency == "" {
		return BackupConsistencyApp
	}
	return b.Consistency
}

// QuiesceCommands returns the commands that pause and resume writes. Both are
// empty unless the mode is command.
func (b *BackupSpec) QuiesceCommands() (pre, post []string) {
	if b.QuiesceMode() != BackupQuiesceCommand || b.Quiesce == nil {
		return nil, nil
	}
	return b.Quiesce.Pre, b.Quiesce.Post
}

// QuiesceContainer returns the container to run quiesce commands in. Empty
// means the first container in the app's pod.
func (b *BackupSpec) QuiesceContainer() string {
	if b == nil || b.Quiesce == nil {
		return ""
	}
	return b.Quiesce.Container
}

// IncludedVolumes returns the claims to capture. Empty means every
// PersistentVolumeClaim the app's Helm release owns, which is the default.
func (b *BackupSpec) IncludedVolumes() []string {
	if b == nil || b.Volumes == nil {
		return nil
	}
	return b.Volumes.Include
}

// ExcludedPaths returns the glob patterns to drop from captured volumes.
func (b *BackupSpec) ExcludedPaths() []string {
	if b == nil || b.Volumes == nil {
		return nil
	}
	return b.Volumes.ExcludePaths
}

// BoundSecretRefs returns the secrets that must travel with this app's data.
func (b *BackupSpec) BoundSecretRefs() []BackupBoundSecret {
	if b == nil {
		return nil
	}
	return b.BoundSecrets
}

// RestoreCommands returns the commands to run once this app's data is loaded,
// and the single command that must succeed before the restore is reported
// successful.
func (b *BackupSpec) RestoreCommands() (post [][]string, verify []string) {
	if b == nil || b.Restore == nil {
		return nil, nil
	}
	return b.Restore.Post, b.Restore.Verify
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
