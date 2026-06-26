// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import "fmt"

// MapperTemplate describes one Keycloak protocol mapper on a client scope.
type MapperTemplate struct {
	// KeycloakName overrides the mapper name in Keycloak when it differs from
	// the catalog template key (e.g. openDesk uses "full name", not "full_name").
	KeycloakName   string            `json:"keycloakName,omitempty"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

// Pack is the OpenDesk-style OIDC configuration applied per tenant realm.
type Pack struct {
	ScopeName        string   `json:"scopeName"`
	ScopeDescription string   `json:"scopeDescription"`
	ClientRole       string   `json:"clientRole"`
	LDAPGroup        string   `json:"ldapGroup"`
	PublicClient     bool     `json:"publicClient"`
	FullScopeAllowed bool     `json:"fullScopeAllowed"`
	DefaultScopes    []string `json:"defaultScopes"`
	Mappers          []string `json:"mappers"`
}

func validatePack(clientID string, pack Pack, templates map[string]MapperTemplate) error {
	if pack.ScopeName == "" || pack.ClientRole == "" || pack.LDAPGroup == "" {
		return fmt.Errorf("oidc pack %q: scopeName, clientRole, and ldapGroup are required", clientID)
	}
	for _, name := range pack.Mappers {
		if _, ok := templates[name]; !ok {
			return fmt.Errorf("oidc pack %q: unknown mapper template %q", clientID, name)
		}
	}
	return nil
}
