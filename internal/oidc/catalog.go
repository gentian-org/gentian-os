/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
