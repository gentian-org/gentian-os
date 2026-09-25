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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ComponentClass is who a component serves, and therefore who is responsible
// for it. It was called ComponentTenancy, with the values system, shared and
// tenant: a placement word, a bare adjective and a scope word for one
// question. "system" named the namespace rather than the role, while its own
// description -- serves contracts to other components, serves no human -- is
// the definition of a service.
//
// +kubebuilder:validation:Enum=service;app;shared-app
type ComponentClass string

const (
	// ComponentClassService serves other components over contracts, and
	// operators at its own console. One instance, in system-<function>,
	// the platform administrator's.
	ComponentClassService ComponentClass = "service"
	// ComponentClassApp serves the people of one tenant, from a dedicated
	// release in tenant-<t>.
	ComponentClassApp ComponentClass = "app"
	// ComponentClassSharedApp is one backend in shared-<app> serving several
	// tenants, each of which still installs the app for itself.
	ComponentClassSharedApp ComponentClass = "shared-app"
)

// ComponentLaunch is how a person reaches a component. It exists because the
// schema could not otherwise tell a component that is deliberately
// unadvertised from one whose tile was forgotten, and an app nobody can open
// is installed and lost rather than refused.
//
// +kubebuilder:validation:Enum=tile;from;none
type ComponentLaunch string

const (
	// ComponentLaunchTile means at least one exposure carries a tile.
	ComponentLaunchTile ComponentLaunch = "tile"
	// ComponentLaunchFrom means another component opens it: Collabora from
	// Nextcloud, a viewer from a file manager. LaunchFrom names the contract.
	ComponentLaunchFrom ComponentLaunch = "from"
	// ComponentLaunchNone means nothing opens it: either it is the launcher
	// itself, which is the desktop, or it has no human surface at all.
	ComponentLaunchNone ComponentLaunch = "none"
)

// ComponentProfileSpec is one catalogue entry: what a component is, what it
// needs from the platform, and what it may expose. The presentation of a
// catalogue listing stays the store's, outside the cluster. The one piece of
// presentation an entry carries here is the tile on an exposure, because the
// portal has to be able to show what the cluster routes without asking the
// store, and because an app that cannot appear on the portal until a service
// outside the cluster answers is an app a person cannot reach. Nothing in it
// grants anything: every permissive statement here is a request that a named
// person answers on the Component.
//
// +kubebuilder:validation:XValidation:rule="!has(self.classes) || !('service' in self.classes) || self.classes.size() == 1",message="service is exclusive: a component may not be both a service and an app"
// +kubebuilder:validation:XValidation:rule="!has(self.classes) || !('shared-app' in self.classes) || self.trustTier == 'platform'",message="shared-app requires trustTier platform"
// +kubebuilder:validation:XValidation:rule="!has(self.expose) || self.expose.all(e, !(has(e.forwardToken) && e.forwardToken) || self.trustTier == 'platform')",message="forwardToken requires trustTier platform: the edge token is valid at the director and at every sibling"
// A service may expose -- a console is not a contract surface -- but only on
// the gateway. The perimeter has no session, and a service console published
// there is never what anybody meant.
// +kubebuilder:validation:XValidation:rule="!has(self.classes) || !('service' in self.classes) || !has(self.expose) || self.expose.all(e, e.surface == 'gateway')",message="a service exposes on the gateway only: the perimeter has no session"
// +kubebuilder:validation:XValidation:rule="!has(self.classes) || !('service' in self.classes) || !has(self.expose) || self.expose.all(e, !has(e.tile) || e.tile.object == 'cluster')",message="a service's tile asks on the cluster: it has no app object and runs in no tenant"
// +kubebuilder:validation:XValidation:rule="!has(self.classes) || ('app' in self.classes) || !has(self.defaultForTenants) || !self.defaultForTenants",message="defaultForTenants is for class app: a service and a shared-app have one instance"
// Exactly one, and nothing outside package to reach for. The OR this replaces
// admitted a chart beside an API integration, and deploymentMethod could
// contradict whichever was set.
// +kubebuilder:validation:XValidation:rule="[has(self.__package__.chart), has(self.__package__.composition) && self.__package__.composition.size() > 0, has(self.__package__.api), has(self.__package__.addon)].exists_one(x, x)",message="a package is exactly one of chart, composition, api or addon"
// +kubebuilder:validation:XValidation:rule="!has(self.customization) || !has(self.customization.addon)",message="an addon is package.addon on this kind, not customization.addon"
// +kubebuilder:validation:XValidation:rule="self.launch != 'from' || (has(self.launchFrom) && self.launchFrom.size() > 0)",message="launch from needs launchFrom: which contract's provider opens this"
// +kubebuilder:validation:XValidation:rule="self.launch == 'from' || !has(self.launchFrom) || self.launchFrom.size() == 0",message="launchFrom is meaningless unless launch is from"
// +kubebuilder:validation:XValidation:rule="self.launch != 'tile' || (has(self.expose) && self.expose.exists(e, has(e.tile)))",message="launch tile needs a tile on an exposure: an app nobody can open is installed and lost"
// +kubebuilder:validation:XValidation:rule="self.launch == 'tile' || !has(self.expose) || !self.expose.exists(e, has(e.tile))",message="a tile means launch tile: say how a person reaches this"
type ComponentProfileSpec struct {
	// Classes lists the modes this component may be deployed under. It is a
	// certification claim, not a choice: whether a component *can* serve
	// several tenants safely is reviewed with the entry, while running it
	// shared is a decision a named person makes on the Component.
	// "service" is exclusive.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	Classes []ComponentClass `json:"classes"`

	// Launch is how a person reaches this component. Required, with no
	// default, for the reason authMode, surface and trustTier are: an app
	// nobody can open should be refused at admission, and "nobody opens this"
	// has to be a word somebody wrote rather than a field they omitted.
	Launch ComponentLaunch `json:"launch"`

	// LaunchFrom names the contract whose provider opens this component.
	// Required when Launch is "from", meaningless otherwise. It records what
	// the model could not say before: which component opens this one.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	LaunchFrom string `json:"launchFrom,omitempty"`

	// TrustTier is the review level of this entry. "platform" is required
	// before "shared-app" may appear in Classes. Required, with no default:
	// the tier is a statement a reviewer made, not an absence.
	TrustTier TrustTier `json:"trustTier"`

	// Version is the catalogue entry's version, not the upstream project's.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`

	// Package is how the component is deployed.
	Package PackageSpec `json:"package"`

	// Requires is what the platform is obliged to provide before this component
	// may run. Unmet means it does not start, and the platform is at fault.
	// +optional
	Requires *RequirementSpec `json:"requires,omitempty"`

	// Integrations are opportunistic relationships with peer components. An
	// absent peer is normal. Each binding is subject to the tenant's grant.
	// +optional
	Integrations []IntegrationRef `json:"integrations,omitempty"`

	// Provides lists the contracts this component supplies. The class decides
	// the audience: cluster-wide for a service and a shared-app, within the
	// tenant otherwise.
	// +optional
	Provides []ContractRef `json:"provides,omitempty"`

	// Secrets are values the platform generates and holds that have no external
	// counterparty. Credentials that arrive with a granted requirement are not
	// declared here; they come with the grant.
	// +optional
	Secrets *ComponentSecrets `json:"secrets,omitempty"`

	// Expose declares entry points. A service may have them -- a console is
	// not a contract surface -- but only on the gateway, never the perimeter,
	// which has no session. Every entry states its authMode; there is no
	// default.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	Expose []ExposureSpec `json:"expose,omitempty"`

	// SessionMaxAge caps the app's own session, for apps that run their own
	// login. The edge bounds reachability; it does not refresh the groups an
	// app captured at its own login, so this — not the access-token lifetime —
	// bounds what a user may still do inside the app after their rights change.
	// +optional
	SessionMaxAge *metav1.Duration `json:"sessionMaxAge,omitempty"`

	// Extensions are containers shipped inside the component's own pod: the
	// only escape hatch, and one that cannot create a cluster-scoped object.
	// +optional
	Extensions []AppSidecarSpec `json:"extensions,omitempty"`

	// Hooks are lifecycle actions at install, upgrade and resync.
	// +optional
	Hooks *HookSpec `json:"hooks,omitempty"`

	// Backup declares what a consistent copy of this component consists of.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// Customization declares what a tenant may change about this component.
	// +optional
	Customization *CustomizationSurface `json:"customization,omitempty"`

	// DefaultForTenants means every tenant gets a Component of this profile,
	// created by the operator when the tenant is provisioned and named after
	// the profile. It is how the desktop reaches every tenant without anyone
	// declaring it, and it is here as a field so that a second component the
	// platform ships to everyone, such as the administration console, is
	// declared the same way rather than by another name the operator knows.
	// Only a profile of class "app" may say it: a service and a shared-app
	// have one instance, so there is nothing to give each tenant.
	// +optional
	DefaultForTenants bool `json:"defaultForTenants,omitempty"`
}

// PackageSpec is the chart, the deployment method, and the mapping from granted
// requirements onto chart values.
// PackageSpec is what this entry is made of, and it is exactly one thing.
// Delivery -- whether the platform runs the component or only routes to it --
// is read from here rather than stated beside it, because a second statement
// of one fact is a second statement that can be wrong. There was one:
// deploymentMethod could say "api" beside a chart, and nothing refused it.
type PackageSpec struct {
	// Chart is the Helm chart or OCI artifact. Delivery: workload.
	// +optional
	Chart *ChartRef `json:"chart,omitempty"`

	// Composition names a Crossplane composition, for a component that is not
	// a single chart. Delivery: workload.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Composition string `json:"composition,omitempty"`

	// API describes a component the platform routes to rather than runs. It
	// does not mean external: the catalogue's own example points at a Service
	// inside the cluster. Delivery: api.
	// +optional
	API *APIIntegration `json:"api,omitempty"`

	// Addon describes a component the platform does not deploy at all: it
	// flips a switch inside another component's own addon system. Delivery:
	// addon. It lived under customization.addon, which is why the one-of rule
	// had to reach out of this struct to finish itself.
	// +optional
	Addon *PackageAddon `json:"addon,omitempty"`

	// ValueMapping places what the platform granted into the chart's values.
	// +optional
	ValueMapping *ValueMapping `json:"valueMapping,omitempty"`

	// ExtraValues are static chart values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`
}

// PackageAddon is a component that rides on another component's addon system.
type PackageAddon struct {
	// ID is what the hosting component's own addon system calls this addon:
	// an Odoo module name, a Nextcloud app id, an Activepieces piece name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`
	ID string `json:"id"`

	// Of is the ComponentProfile this addon activates into.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Of string `json:"of"`
}

// RequirementSpec is everything the platform must provide or permit.
type RequirementSpec struct {
	// Services are what the platform must fulfil before this component runs:
	// identity, database, object storage, cache, mail, MCP. Each is fulfilled
	// by a component of class "service", or by something outside the cluster
	// entirely -- a relay, a bucket at a cloud provider. It was called
	// kernelRequirements, which named a fulfiller that is not the fulfiller,
	// and then contracts, which is the word the open named set in Provides
	// already holds.
	// +optional
	Services *ServiceRequirements `json:"services,omitempty"`

	// Privileges escape the default posture. Declaring one is asking, never
	// receiving: each is granted per install, by a named person, and recorded
	// on the Component.
	// +optional
	Privileges *PrivilegeRequest `json:"privileges,omitempty"`
}

// PrivilegeRequest is what a profile asks for beyond the default posture. Who
// approves follows from the kind of privilege, never from a field, or a profile
// could ask for the cheaper approver.
type PrivilegeRequest struct {
	// PodSecurity waives a named admission policy for a named part of the
	// component. Cluster scope: it weakens a rule that protects the node.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	PodSecurity []PodSecurityWaiver `json:"podSecurity,omitempty"`

	// Egress opens outbound network beyond the tenant baseline. Tenant scope:
	// the traffic leaves the tenant's own namespace.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	Egress []EgressRequest `json:"egress,omitempty"`

	// ClusterRoles the component's ServiceAccount needs. Cluster scope, and the
	// rarest: an app that needs the Kubernetes API is most of the way to being
	// a controller.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	ClusterRoles []ClusterRoleRequest `json:"clusterRoles,omitempty"`
}

// PodSecurityWaiver asks for one admission policy to be waived.
type PodSecurityWaiver struct {
	// Name is what a PrivilegeGrant refers to. Unique within the profile.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Policy is the ClusterPolicy this waives.
	// +kubebuilder:validation:MinLength=1
	Policy string `json:"policy"`
	// Scope is the container or composition part it is waived for.
	// +kubebuilder:validation:MinLength=1
	Scope string `json:"scope"`
	// Reason is what the approver reads.
	// +kubebuilder:validation:MinLength=10
	Reason string `json:"reason"`
}

// EgressRequest asks for outbound network beyond the tenant baseline.
type EgressRequest struct {
	// Name is what a PrivilegeGrant refers to. Unique within the profile.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Rule is the egress to allow.
	Rule networkingv1.NetworkPolicyEgressRule `json:"rule"`
	// Reason is what the approver reads.
	// +kubebuilder:validation:MinLength=10
	Reason string `json:"reason"`
}

// ClusterRoleRequest asks for Kubernetes API access.
type ClusterRoleRequest struct {
	// Name is what a PrivilegeGrant refers to. Unique within the profile.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinItems=1
	Rules []rbacv1.PolicyRule `json:"rules"`
	// Reason is what the approver reads.
	// +kubebuilder:validation:MinLength=10
	Reason string `json:"reason"`
}

// ComponentSecrets are the secrets a component declares.
type ComponentSecrets struct {
	// Generated secrets are random, created once and held in the vault. The
	// robust default.
	// +optional
	Generated []AppSecret `json:"generated,omitempty"`

	// Derived secrets are recomputed from the tenant and component name. Their
	// formula and inputs are load-bearing forever: a change to either silently
	// rotates the value for every existing tenant. Prefer Generated.
	// +optional
	Derived []DerivedSecretKey `json:"derived,omitempty"`
}

// SurfaceKind is where an exposure entry is published.
// +kubebuilder:validation:Enum=gateway;perimeter
type SurfaceKind string

const (
	// SurfaceGateway is the authenticated tenant gateway. Always on: the entry
	// carries the session.
	SurfaceGateway SurfaceKind = "gateway"
	// SurfacePerimeter is a publishing proxy in tenant-<t>-dmz with its own
	// credential. Off until a perimeter approver enables it on the Component.
	SurfacePerimeter SurfaceKind = "perimeter"
)

// AuthMode is how a caller of an exposure entry is authenticated.
// +kubebuilder:validation:Enum=oidc;jwt;bearer;basic;signature;none
type AuthMode string

const (
	AuthModeOIDC      AuthMode = "oidc"
	AuthModeJWT       AuthMode = "jwt"
	AuthModeBearer    AuthMode = "bearer"
	AuthModeBasic     AuthMode = "basic"
	AuthModeSignature AuthMode = "signature"
	// AuthModeNone is explicit so that it is a word someone wrote and a
	// reviewer can find.
	AuthModeNone AuthMode = "none"
)

// ExposureSpec declares one entry point.
//
// +kubebuilder:validation:XValidation:rule="!(self.surface == 'perimeter' && has(self.forwardToken) && self.forwardToken)",message="forwardToken is meaningless on a perimeter entry: it has no session"
// +kubebuilder:validation:XValidation:rule="self.surface != 'perimeter' || self.authMode != 'oidc'",message="a perimeter entry cannot use authMode oidc: the session lives on the gateway"
type ExposureSpec struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// Surface selects where this entry is published.
	Surface SurfaceKind `json:"surface"`

	// AuthMode is mandatory and has no default.
	AuthMode AuthMode `json:"authMode"`

	// SubDomain is the host label of this entry in the tenant's zone. Empty
	// means the component's own name.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	SubDomain string `json:"subDomain,omitempty"`

	// Paths this entry serves. Empty means the whole host.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	Paths []string `json:"paths,omitempty"`

	// DenyPaths are refused even where Paths admits them. Deny wins regardless
	// of specificity, so a broad allow with narrow denials stays readable.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	DenyPaths []string `json:"denyPaths,omitempty"`

	// StripPrefix removes the matched path prefix before the request reaches
	// the backend.
	// +optional
	StripPrefix bool `json:"stripPrefix,omitempty"`

	// Source restricts who may call this entry, before authMode is considered.
	// +optional
	Source *SourceRestriction `json:"source,omitempty"`

	// ForwardToken asks the gateway to pass the edge access token to this
	// backend. A backend otherwise gets identity headers, not a bearer that is
	// also valid at the director and at every sibling.
	// +optional
	ForwardToken bool `json:"forwardToken,omitempty"`

	// Tile is how this entry appears on the portal, for an entry a person is
	// meant to open. An entry that declares none is reachable and unadvertised,
	// which is what an API or a callback endpoint should be, so the absence is
	// the default and nothing has to opt out.
	// +optional
	Tile *ExposureTile `json:"tile,omitempty"`

	// Backend is the Service the entry routes to.
	Backend BackendRef `json:"backend"`
}

// ExposureTile is what a component says about itself on the portal.
//
// It is here rather than in the store because the portal must be able to show
// an app the moment the cluster routes it, and because the operator already
// holds everything else a tile needs: the host it wrote the route for, and the
// tenant the component runs in. What the store adds is the catalogue listing a
// person browses before installing, which is a different page with a different
// audience.
//
// Nothing here grants anything. Relation names the question the director asks
// the graph about the caller before the tile is put on their page, so a tile
// nobody may open is a tile nobody is shown.
type ExposureTile struct {
	// DisplayName is the label under the icon. A few words, in the language of
	// the person using it rather than of the chart that installs it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	DisplayName string `json:"displayName"`

	// Description is the sentence the portal shows beside the label.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`

	// Icon is the glyph the portal draws, by name. The portal owns the set; a
	// name it does not know draws its fallback rather than failing the page.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Icon string `json:"icon"`

	// Path is where within the host the tile leads. Empty means the front page.
	// It exists because an app's front page is not always its entry: a tool
	// whose front page is a login form is better entered past it, since the
	// person following the tile already holds the zone's session.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path,omitempty"`

	// Relation is what the caller must hold, on the object Object names, for the
	// tile to be on their page: a permission of the authorization model
	// (authz/model/v1/model.fga). It is required: a tile with no question is a
	// link shown to everyone, and the portal is not where that is decided.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^can_[a-z0-9_]+$`
	Relation string `json:"relation"`

	// Object is what Relation is checked against. "app", the default, is
	// this component's own app object, app:<tenant>/<profile>, where an
	// installed app's permissions live. "tenant" is the tenant the component
	// runs in, tenant:<tenant>, which is where the model keeps the permissions
	// that are about administering the tenant rather than using an app in it.
	// The model says it in one line: admin tiles are tenant#can_administer.
	// A component that exists to administer the tenant it runs in has no app
	// object worth asking about, and asking one would be answered no.
	//
	// "cluster" is the third, for a service's own console: a service has no
	// app object and runs in no tenant, and the relations that govern it --
	// can_operate_system, can_configure, can_audit -- are all on cluster.
	//
	// Not called "on": YAML 1.1 reads a bare on as the boolean true, so a
	// profile written by hand would carry a key named true and be refused by
	// the schema, and the same goes for off, yes and no.
	// +optional
	// +kubebuilder:default=app
	// +kubebuilder:validation:Enum=app;tenant;cluster
	Object TileObject `json:"object,omitempty"`
}

// TileObject is the kind of object a tile's relation is checked against.
type TileObject string

const (
	// TileObjectApp checks the relation on app:<tenant>/<profile>.
	TileObjectApp TileObject = "app"
	// TileObjectTenant checks the relation on tenant:<tenant>.
	TileObjectTenant TileObject = "tenant"
	// TileObjectCluster checks the relation on cluster:<cluster>, which is
	// where a service's console is governed from.
	TileObjectCluster TileObject = "cluster"
)

// SourceRestriction pins the caller. Exactly one form; both are evaluated at
// the proxy, not the app.
//
// +kubebuilder:validation:XValidation:rule="(has(self.cidrs) && self.cidrs.size() > 0) != (has(self.component) && self.component.size() > 0)",message="exactly one of cidrs or component"
type SourceRestriction struct {
	// CIDRs admitted, after the real client IP is resolved at the edge.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=43
	CIDRs []string `json:"cidrs,omitempty"`

	// Component names another installed component whose pods may call this
	// entry. Preferred over CIDRs, which age badly.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Component string `json:"component,omitempty"`
}

// BackendRef is the Service an exposure routes to. It defaults to this
// component's own namespace, and may name another component's instead.
//
// Three cases need the second form and only one of them is new: an addon has
// no Service of its own and its tile points into its base; a shared-app
// binding routes into shared-<app>, which is required today and is implied by
// fulfilment rather than declared; and everything else routes to itself.
type BackendRef struct {
	// Component names another component whose Service this entry routes to.
	// Empty means this component's own. Admission bounds it to a component
	// this one is already bound to -- its addon base, or the shared instance
	// it is bound to -- so publishing is never a way to reach a component
	// there is no relationship with.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Component string `json:"component,omitempty"`

	// +kubebuilder:validation:MinLength=1
	Service string `json:"service"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// HookSpec are lifecycle actions.
type HookSpec struct {
	// PostInstall runs once after the component is first ready.
	// +optional
	PostInstall *AppPostInstallJob `json:"postInstall,omitempty"`

	// Provisioning configures the component through its own API after install
	// and on resync.
	// +optional
	Provisioning *ProvisioningSpec `json:"provisioning,omitempty"`
}

// ComponentProfileStatus is the observed state of a ComponentProfile.
type ComponentProfileStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Delivery is read from the package: workload for a chart or a
	// composition, api for a component the platform routes to, addon for one
	// it does not deploy at all. It is status and not spec on purpose -- a
	// second settable statement of one fact is a second statement that can be
	// wrong, and deploymentMethod was exactly that. Here so a person can see
	// at a glance which entries the platform runs and which it merely routes
	// to, which is the question behind the exposure register.
	// +optional
	Delivery ComponentDelivery `json:"delivery,omitempty"`
}

// ComponentDelivery is how the platform realises a component, derived from
// which member of the package union is set.
// +kubebuilder:validation:Enum=workload;api;addon
type ComponentDelivery string

const (
	// ComponentDeliveryWorkload is a chart or a composition: the platform runs it.
	ComponentDeliveryWorkload ComponentDelivery = "workload"
	// ComponentDeliveryAPI is a component already running elsewhere, which the
	// platform routes to. It does not mean external: the catalogue's own
	// example points at a Service inside the cluster.
	ComponentDeliveryAPI ComponentDelivery = "api"
	// ComponentDeliveryAddon is a switch inside another component's own addon
	// system. Nothing is run and nothing is routed.
	ComponentDeliveryAddon ComponentDelivery = "addon"
)

// Delivery reads the package union. It is the one place that mapping lives.
func (s ComponentProfileSpec) Delivery() ComponentDelivery {
	switch {
	case s.Package.API != nil:
		return ComponentDeliveryAPI
	case s.Package.Addon != nil:
		return ComponentDeliveryAddon
	default:
		return ComponentDeliveryWorkload
	}
}

// ComponentProfile is a catalogue entry for a system service, an app or an
// agent. The cluster holds only the profiles something installed refers to.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cprof
// +kubebuilder:printcolumn:name="Classes",type=string,JSONPath=`.spec.classes`
// +kubebuilder:printcolumn:name="Delivery",type=string,JSONPath=`.status.delivery`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.trustTier`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
type ComponentProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComponentProfileSpec   `json:"spec,omitempty"`
	Status ComponentProfileStatus `json:"status,omitempty"`
}

// ComponentProfileList contains a list of ComponentProfile.
//
// +kubebuilder:object:root=true
type ComponentProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComponentProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ComponentProfile{}, &ComponentProfileList{})
}
