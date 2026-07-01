/*
Copyright 2026 The Gentian Authors.

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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OIDCPackCatalog is a cluster-scoped catalogue of OIDC packs for tenant apps.
// Shipped from gentian-apps (profiles/<app>/oidc-catalog.yaml) and consumed
// by the operator and app-default composition for pack Jobs and ClientDefaultScopes.
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

	// ExtraManagedByAttributeGroups lists additional cn=managed-by-attribute-* groups
	// provisioned per tenant that are not tied to an OIDC pack entitlementGroup (e.g.
	// openDesk portal admin groups).
	// +optional
	ExtraManagedByAttributeGroups []string `json:"extraManagedByAttributeGroups,omitempty"`
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

// OIDCPackSpec is the OpenDesk-style OIDC configuration for one clientId.
type OIDCPackSpec struct {
	// ScopeName is the Keycloak client scope created for this pack.
	// +kubebuilder:validation:Required
	ScopeName string `json:"scopeName"`

	// ScopeDescription is shown in the Keycloak admin UI.
	// +optional
	ScopeDescription string `json:"scopeDescription,omitempty"`

	// ClientRole is the Keycloak client role mapped from the entitlement group.
	// +kubebuilder:validation:Required
	ClientRole string `json:"clientRole"`

	// EntitlementGroup is the Keycloak group name mapped to the pack client role.
	// +kubebuilder:validation:Required
	EntitlementGroup string `json:"entitlementGroup"`

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
type OIDCPackCatalogStatus struct {
	// PackCount is the number of entries in spec.packs.
	// +optional
	PackCount int `json:"packCount,omitempty"`
}

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
