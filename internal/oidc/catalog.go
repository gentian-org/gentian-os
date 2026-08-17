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


package oidc

import "fmt"

// MapperTemplate describes one Keycloak protocol mapper on a client scope.
type MapperTemplate struct {
	// KeycloakName overrides the mapper name in Keycloak when it differs from
	// the catalog template key (e.g. some catalogs use "full name", not "full_name").
	KeycloakName   string            `json:"keycloakName,omitempty"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

// Pack is the OIDC configuration applied per tenant realm for one clientId.
type Pack struct {
	ServiceClient    bool     `json:"serviceClient"`
	ScopeName        string   `json:"scopeName"`
	ScopeDescription string   `json:"scopeDescription"`
	ClientRole       string   `json:"clientRole"`
	EntitlementGroup string   `json:"entitlementGroup"`
	PublicClient     bool     `json:"publicClient"`
	FullScopeAllowed bool     `json:"fullScopeAllowed"`
	DefaultScopes    []string `json:"defaultScopes"`
	Mappers          []string `json:"mappers"`
}

func validatePack(clientID string, pack Pack, templates map[string]MapperTemplate) error {
	// A service client provisions credentials and nothing else, so the three
	// app-shaped fields are meaningless for it rather than merely optional.
	if pack.ServiceClient {
		if pack.PublicClient {
			return fmt.Errorf("oidc pack %q: serviceClient cannot be publicClient — it needs a secret to authenticate to the introspection endpoint", clientID)
		}
		if len(pack.Mappers) > 0 {
			return fmt.Errorf("oidc pack %q: serviceClient creates no client scope, so it cannot carry mappers", clientID)
		}
		return nil
	}
	if pack.ScopeName == "" || pack.ClientRole == "" || pack.EntitlementGroup == "" {
		return fmt.Errorf("oidc pack %q: scopeName, clientRole, and entitlementGroup are required", clientID)
	}
	for _, name := range pack.Mappers {
		if _, ok := templates[name]; !ok {
			return fmt.Errorf("oidc pack %q: unknown mapper template %q", clientID, name)
		}
	}
	return nil
}
