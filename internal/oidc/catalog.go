// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"embed"
	"fmt"
	"sync"

	"sigs.k8s.io/yaml"
)

//go:embed packs
var packFS embed.FS

var (
	loadOnce       sync.Once
	loadErr        error
	cachedByCID    map[string]Pack
	cachedTemplates map[string]MapperTemplate
)

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

type catalogFile struct {
	Spec struct {
		MapperTemplates map[string]MapperTemplate `json:"mapperTemplates"`
		Packs           map[string]Pack           `json:"packs"`
	} `json:"spec"`
}

// LoadCatalog parses embedded OIDC pack definitions. Results are cached.
func LoadCatalog() (map[string]Pack, map[string]MapperTemplate, error) {
	loadOnce.Do(func() {
		raw, err := packFS.ReadFile("packs/opendesk.yaml")
		if err != nil {
			loadErr = fmt.Errorf("read opendesk oidc packs: %w", err)
			return
		}
		var doc catalogFile
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			loadErr = fmt.Errorf("parse opendesk oidc packs: %w", err)
			return
		}
		cachedByCID = make(map[string]Pack, len(doc.Spec.Packs))
		cachedTemplates = make(map[string]MapperTemplate, len(doc.Spec.MapperTemplates))
		for k, v := range doc.Spec.MapperTemplates {
			cachedTemplates[k] = v
		}
		for clientID, pack := range doc.Spec.Packs {
			p := pack
			if err := validatePack(clientID, p, cachedTemplates); err != nil {
				loadErr = err
				return
			}
			cachedByCID[clientID] = p
		}
	})
	if loadErr != nil {
		return nil, nil, loadErr
	}
	out := make(map[string]Pack, len(cachedByCID))
	for k, v := range cachedByCID {
		out[k] = v
	}
	templates := make(map[string]MapperTemplate, len(cachedTemplates))
	for k, v := range cachedTemplates {
		templates[k] = v
	}
	return out, templates, nil
}

// PackForClient returns the pack for an OIDC clientId, if registered.
func PackForClient(clientID string) (Pack, map[string]MapperTemplate, bool, error) {
	packs, templates, err := LoadCatalog()
	if err != nil {
		return Pack{}, nil, false, err
	}
	pack, ok := packs[clientID]
	return pack, templates, ok, nil
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
