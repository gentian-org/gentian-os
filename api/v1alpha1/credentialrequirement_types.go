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
)

// CredentialRequirement declares a secret that must be supplied from outside the
// cluster: a registry token, a DNS provider key, an SMTP relay password. It is
// catalogue data, not a provisioning API — a schema, a target path in OpenBao,
// and a validation hint.
//
// It is deliberately a plain CRD rather than a Crossplane XRD. It composes
// nothing: an XRD requires a Composition producing managed resources, and there
// are none to produce. It sits beside AppProfile as catalogue.
//
// There is no controller. Each requirement emits an ExternalSecret, and ESO's
// sync status IS the satisfaction probe — path absent in OpenBao gives
// SecretSyncedError, path present gives Ready. That keeps satisfaction a
// Kubernetes condition (so Compositions can gate on it) and means no bespoke
// component has to hold an OpenBao token in order to poll.
//
// Naming: these are "credential requirements" or "credential inputs", never
// "external secrets". ESO's ExternalSecret is a reference to a secret stored
// externally, which is nearly the opposite of a secret that must be supplied
// from outside.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=credreq
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Optional",type=boolean,JSONPath=`.spec.optional`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.vaultPath`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CredentialRequirement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CredentialRequirementSpec `json:"spec,omitempty"`
}

// CredentialRequirementSpec describes one credential an operator must supply.
//
// +kubebuilder:validation:XValidation:rule="self.scope == 'tenant' ? (has(self.tenant) && self.tenant != ”) : (!has(self.tenant) || self.tenant == ”)",message="tenant is required when scope is tenant, and must be empty when scope is cluster"
type CredentialRequirementSpec struct {
	// DisplayName is the label a form renders for this requirement.
	// +kubebuilder:validation:MinLength=1
	DisplayName string `json:"displayName"`

	// Description explains what the credential is for and where to obtain it.
	// It is shown to an operator who may not have installed the cluster.
	// +optional
	Description string `json:"description,omitempty"`

	// Phase decides whether an unsatisfied requirement blocks installation.
	//
	// bootstrap: the installer prompts for it and will not proceed without it.
	// runtime:   deferrable to the on-cluster credential manager.
	//
	// The bootstrap set is expected to number under five, and every member must
	// be validatable with curl or openssl alone — anything needing an SDK or a
	// signing algorithm is runtime by that fact, which is what keeps the
	// installer's credential logic from growing.
	// +kubebuilder:validation:Enum=bootstrap;runtime
	Phase string `json:"phase"`

	// Scope determines which OpenBao policy governs the write and who may see
	// the requirement. A tenant admin must not reach cluster-scoped paths.
	// +kubebuilder:validation:Enum=cluster;tenant
	Scope string `json:"scope"`

	// Tenant names the tenant a tenant-scoped requirement belongs to.
	//
	// Scope alone is a class, not an identity: it separates "a tenant may see
	// this" from "an operator may see this", but says nothing about WHICH
	// tenant. Without this field every tenant admin sees every tenant-scoped
	// requirement, which for a credential to a tenant-proprietary repository is
	// a disclosure rather than an inconvenience.
	//
	// Empty for cluster scope, and required for tenant scope — the CEL rule on
	// this type rejects either mistake at admission, because a tenant-scoped
	// requirement with no tenant would otherwise be visible to all of them.
	// +optional
	Tenant string `json:"tenant,omitempty"`

	// Optional marks an unsatisfied requirement as an informational gap rather
	// than an error.
	// +optional
	Optional bool `json:"optional,omitempty"`

	// VaultPath is the only coupling between this requirement and the storage
	// layer. Several requirements may share a path when one credential serves
	// several consumers; rotation is then a single write.
	// +kubebuilder:validation:MinLength=1
	VaultPath string `json:"vaultPath"`

	// Fields enumerates the keys stored at VaultPath.
	// +kubebuilder:validation:MinItems=1
	Fields []CredentialField `json:"fields"`

	// Validate is the probe run BEFORE the value is written, so a bad paste
	// becomes a red field at install time rather than a stalled provisioning
	// job hours later.
	// +optional
	Validate *CredentialValidation `json:"validate,omitempty"`

	// ConsumedBy documents which resources depend on this credential. It drives
	// impact analysis for rotation and is not enforced.
	// +optional
	ConsumedBy []CredentialConsumer `json:"consumedBy,omitempty"`
}

// CredentialField is one key at the requirement's vault path.
type CredentialField struct {
	// Key is the field name within the OpenBao secret.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Format hints at how a form should render and check the field.
	// +kubebuilder:validation:Enum=string;password;token;email;url;hostname;port
	// +kubebuilder:default=string
	Format string `json:"format,omitempty"`

	// Secret marks a field whose value must never be displayed or logged.
	// +optional
	Secret bool `json:"secret,omitempty"`

	// MinLength enforces a minimum, declared once here rather than hardcoded at
	// each site that checks it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinLength int `json:"minLength,omitempty"`

	// Example is a non-secret illustration of the expected shape.
	// +optional
	Example string `json:"example,omitempty"`
}

// CredentialValidation selects the pre-write probe.
type CredentialValidation struct {
	// Type keys the validator.
	//
	// Only these are implementable in curl or openssl, which is the ceiling on
	// what the installer validates. s3 and dns-provider need request signing or
	// a provider SDK, so credentials using them are runtime by construction and
	// are validated by the on-cluster credential manager instead.
	//
	// noop is rejected for phase: bootstrap — a bootstrap credential with no
	// probe is a design error, resolved by reclassifying it as runtime.
	// cloudflare-dns looks the kernel domain's zone up through the Cloudflare
	// API: one authenticated GET, so it stays inside the curl-only ceiling, and
	// it answers whether the token can reach the zone DNS-01 must write to
	// rather than what kind of token it is.
	// +kubebuilder:validation:Enum=oci-registry;git-https;oidc-discovery;cloudflare-dns;smtp;noop
	Type string `json:"type"`

	// Host is the endpoint probed, when the validator needs one that is not
	// derivable from the fields themselves.
	// +optional
	Host string `json:"host,omitempty"`

	// URL is the full endpoint probed, for validators that need a path.
	// +optional
	URL string `json:"url,omitempty"`
}

// CredentialConsumer names a resource that depends on this credential.
type CredentialConsumer struct {
	// Kind of the consuming resource, e.g. XRepository.
	Kind string `json:"kind"`

	// Name of the consuming resource.
	Name string `json:"name"`
}

// CredentialRequirementList contains a list of CredentialRequirement.
// +kubebuilder:object:root=true
type CredentialRequirementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialRequirement `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CredentialRequirement{}, &CredentialRequirementList{})
}
