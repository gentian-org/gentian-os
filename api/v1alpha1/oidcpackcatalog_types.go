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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OIDCPackCatalog is a cluster-scoped catalog of OIDC packs for tenant apps.
// Shipped from gentian-apps (profiles/<app>/oidc-catalog.yaml) and consumed
// by the operator for pack Jobs and an app's client default scopes.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=oidcpack;oidcpacks
// +kubebuilder:printcolumn:name="Packs",type=integer,JSONPath=`.status.packCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type OIDCPackCatalog struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OIDCPackCatalogSpec   `json:"spec,omitempty"`
	Status OIDCPackCatalogStatus `json:"status,omitempty"`
}

// OIDCPackCatalogSpec holds mapper templates and per-clientId pack definitions.
type OIDCPackCatalogSpec struct {
	// MapperTemplates are reusable Keycloak protocol mapper definitions.
	// +optional
	MapperTemplates map[string]OIDCMapperTemplate `json:"mapperTemplates,omitempty"`

	// Packs maps OIDC clientId to pack configuration.
	// +optional
	Packs map[string]OIDCPackSpec `json:"packs,omitempty"`
}

// OIDCMapperTemplate describes one Keycloak protocol mapper on a client scope.
type OIDCMapperTemplate struct {
	// KeycloakName overrides the mapper name in Keycloak when it differs from
	// the catalog template key.
	// +optional
	KeycloakName string `json:"keycloakName,omitempty"`

	// ProtocolMapper is the Keycloak protocol mapper type.
	// +kubebuilder:validation:Required
	ProtocolMapper string `json:"protocolMapper"`

	// Config holds Keycloak mapper configuration key/value pairs.
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// OIDCPackSpec is the OIDC configuration for one clientId.
// Making scopeName and clientRole optional in the schema is what lets a
// service pack omit them; the rule keeps an APP pack from omitting them by
// accident.
// The presence checks are deliberately cheap — CEL costs are estimated against
// worst-case string lengths, and comparing these to the empty string as well pushed the
// rule over the API server's budget. MinLength on each field rejects an empty
// string when the key IS present, so presence is all this needs to test.
// +kubebuilder:validation:XValidation:rule="(has(self.serviceClient) && self.serviceClient) || (has(self.scopeName) && has(self.clientRole))",message="scopeName and clientRole are required unless serviceClient is true"
// +kubebuilder:validation:XValidation:rule="!(has(self.serviceClient) && self.serviceClient) || !(has(self.publicClient) && self.publicClient)",message="a serviceClient pack cannot be a publicClient: it authenticates to the introspection endpoint with a secret"
type OIDCPackSpec struct {
	// ServiceClient provisions a confidential client for a backend service
	// rather than an application users log in to.
	//
	// The default pack shape assumes a user-facing app: a client scope,
	// protocol mappers and a client role. A service that only validates other
	// clients' tokens — kernel Dovecot introspecting XOAUTH2 access tokens is
	// the case this exists for — needs none of that. It needs client
	// credentials and nothing else, and giving it a scope and a role would put
	// an empty role in every realm and imply people can be given access to it.
	//
	// When true, scopeName and clientRole are ignored (and may
	// be empty), the browser flow is disabled on the client, and no scope, mapper
	// or role is created. publicClient must stay false: a service
	// client with no secret cannot authenticate to the introspection endpoint.
	// +optional
	ServiceClient bool `json:"serviceClient,omitempty"`

	// ScopeName is the Keycloak client scope created for this pack.
	// Required unless serviceClient is true.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ScopeName string `json:"scopeName,omitempty"`

	// ScopeDescription is shown in the Keycloak admin UI.
	// +optional
	ScopeDescription string `json:"scopeDescription,omitempty"`

	// ClientRole is the Keycloak client role made on the pack's client. Nobody
	// is given it: who may use the app is the rights store's to say, and the
	// app reads a person's roles from the token's roles claim.
	// Required unless serviceClient is true.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ClientRole string `json:"clientRole,omitempty"`

	// EntitlementGroup is accepted and ignored: no group is made and none
	// is mapped to the client role, since no group of the realm carries a
	// right. Who may use the app is the rights store's to say.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	EntitlementGroup string `json:"entitlementGroup,omitempty"`

	// PublicClient marks the OIDC client as PUBLIC (PKCE, no secret).
	// +optional
	PublicClient bool `json:"publicClient,omitempty"`

	// FullScopeAllowed controls Keycloak fullScopeAllowed on the client.
	// +optional
	FullScopeAllowed bool `json:"fullScopeAllowed,omitempty"`

	// DefaultScopes are realm default scopes attached before the pack scope.
	// +optional
	DefaultScopes []string `json:"defaultScopes,omitempty"`

	// Mappers lists mapper template keys from spec.mapperTemplates.
	// +optional
	Mappers []string `json:"mappers,omitempty"`
}

// OIDCPackCatalogStatus is optional observed state.
type OIDCPackCatalogStatus struct{}

// OIDCPackCatalogList contains a list of OIDCPackCatalog.
// +kubebuilder:object:root=true
type OIDCPackCatalogList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OIDCPackCatalog `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OIDCPackCatalog{}, &OIDCPackCatalogList{})
}
