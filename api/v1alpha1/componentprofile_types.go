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

// ComponentTenancy is a mode a component may be deployed under.
// +kubebuilder:validation:Enum=system;shared;tenant
type ComponentTenancy string

const (
	// ComponentTenancySystem serves contracts to other components from
	// system-<function>. It serves no human and has no exposure.
	ComponentTenancySystem ComponentTenancy = "system"
	// ComponentTenancyShared is one backend in shared-<app> serving several
	// tenants, each of which still installs the app for itself.
	ComponentTenancyShared ComponentTenancy = "shared"
	// ComponentTenancyTenant is a dedicated release in tenant-<t>.
	ComponentTenancyTenant ComponentTenancy = "tenant"
)

// ComponentProfileSpec is one catalogue entry: what a component is, what it
// needs from the platform, and what it may expose. It carries no presentation
// fields — names, descriptions, icons and tiles are the store's, outside the
// cluster — and nothing in it grants anything: every permissive statement here
// is a request that a named person answers on the Component.
//
// +kubebuilder:validation:XValidation:rule="!('system' in self.tenancy) || self.tenancy.size() == 1",message="system is exclusive: a component serving contracts does not also serve humans"
// +kubebuilder:validation:XValidation:rule="!('system' in self.tenancy) || !has(self.expose) || self.expose.size() == 0",message="system components have no exposure"
// +kubebuilder:validation:XValidation:rule="!('shared' in self.tenancy) || self.trustTier == 'platform'",message="shared tenancy requires trustTier platform"
// +kubebuilder:validation:XValidation:rule="!has(self.expose) || self.expose.all(e, !(has(e.forwardToken) && e.forwardToken) || self.trustTier == 'platform')",message="forwardToken requires trustTier platform: the edge token is valid at the director and at every sibling"
// +kubebuilder:validation:XValidation:rule="has(self.package.chart) || (has(self.package.compositionRef) && self.package.compositionRef.size() > 0) || has(self.package.apiIntegration) || (has(self.customization) && has(self.customization.addon))",message="a package is a chart, a composition or an API integration; only an addon, which rides on its base, has none"
type ComponentProfileSpec struct {
	// Tenancy lists the modes this component may be deployed under. It is a
	// certification claim, not a choice. "system" is exclusive.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	Tenancy []ComponentTenancy `json:"tenancy"`

	// TrustTier is the review level of this entry. "platform" is required
	// before "shared" may appear in Tenancy. Required, with no default: the
	// tier is a statement a reviewer made, not an absence.
	TrustTier TrustTier `json:"trustTier"`

	// Version is the catalogue entry's version, not the upstream project's.
	// +kubebuilder:validation:MinLength=1
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

	// Provides lists the contracts this component supplies. Tenancy decides the
	// audience: cluster-wide for system and shared, within the tenant otherwise.
	// +optional
	Provides []ContractRef `json:"provides,omitempty"`

	// Secrets are values the platform generates and holds that have no external
	// counterparty. Credentials that arrive with a granted requirement are not
	// declared here; they come with the grant.
	// +optional
	Secrets *ComponentSecrets `json:"secrets,omitempty"`

	// Expose declares entry points. Forbidden for system tenancy. Every entry
	// states its authMode; there is no default.
	// +optional
	// +listType=map
	// +listMapKey=name
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
}

// PackageSpec is the chart, the deployment method, and the mapping from granted
// requirements onto chart values.
type PackageSpec struct {
	// Chart is the Helm chart or OCI artifact.
	// +optional
	Chart *ChartRef `json:"chart,omitempty"`

	// DeploymentMethod selects how the component is rolled out.
	// +optional
	DeploymentMethod DeploymentMethod `json:"deploymentMethod,omitempty"`

	// CompositionRef names a Crossplane composition for components that are
	// not a single chart.
	// +optional
	CompositionRef string `json:"compositionRef,omitempty"`

	// ValueMapping places what the platform granted into the chart's values.
	// +optional
	ValueMapping *ValueMapping `json:"valueMapping,omitempty"`

	// ExtraValues are static chart values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraValues *runtime.RawExtension `json:"extraValues,omitempty"`

	// APIIntegration describes a component that is an API client rather than a
	// workload.
	// +optional
	APIIntegration *APIIntegration `json:"apiIntegration,omitempty"`
}

// RequirementSpec is everything the platform must provide or permit.
type RequirementSpec struct {
	// Contracts are platform capabilities: identity, database, object storage,
	// cache, mail, LLM, MCP.
	// +optional
	Contracts *KernelRequirements `json:"contracts,omitempty"`

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
	PodSecurity []PodSecurityWaiver `json:"podSecurity,omitempty"`

	// Egress opens outbound network beyond the tenant baseline. Tenant scope:
	// the traffic leaves the tenant's own namespace.
	// +optional
	// +listType=map
	// +listMapKey=name
	Egress []EgressRequest `json:"egress,omitempty"`

	// ClusterRoles the component's ServiceAccount needs. Cluster scope, and the
	// rarest: an app that needs the Kubernetes API is most of the way to being
	// a controller.
	// +optional
	// +listType=map
	// +listMapKey=name
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
	Paths []string `json:"paths,omitempty"`

	// DenyPaths are refused even where Paths admits them. Deny wins regardless
	// of specificity, so a broad allow with narrow denials stays readable.
	// +optional
	// +listType=set
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

	// Backend is the Service the entry routes to.
	Backend BackendRef `json:"backend"`
}

// SourceRestriction pins the caller. Exactly one form; both are evaluated at
// the proxy, not the app.
//
// +kubebuilder:validation:XValidation:rule="(has(self.cidrs) && self.cidrs.size() > 0) != (has(self.component) && self.component.size() > 0)",message="exactly one of cidrs or component"
type SourceRestriction struct {
	// CIDRs admitted, after the real client IP is resolved at the edge.
	// +optional
	// +listType=set
	CIDRs []string `json:"cidrs,omitempty"`

	// Component names another installed component whose pods may call this
	// entry. Preferred over CIDRs, which age badly.
	// +optional
	Component string `json:"component,omitempty"`
}

// BackendRef is a Service in the component's own namespace.
type BackendRef struct {
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
}

// ComponentProfile is a catalogue entry for a system service, an app or an
// agent. The cluster holds only the profiles something installed refers to.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cprof
// +kubebuilder:printcolumn:name="Tenancy",type=string,JSONPath=`.spec.tenancy`
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
