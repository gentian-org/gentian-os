// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResolvePack returns the OIDC pack for clientID from cluster OIDCPackCatalog CRs.
func ResolvePack(ctx context.Context, c client.Reader, clientID string) (Pack, map[string]MapperTemplate, bool, error) {
	if clientID == "" {
		return Pack{}, nil, false, nil
	}
	if c == nil {
		return Pack{}, nil, false, fmt.Errorf("kubernetes client is required")
	}
	return packFromCluster(ctx, c, clientID)
}

func packFromCluster(ctx context.Context, c client.Reader, clientID string) (Pack, map[string]MapperTemplate, bool, error) {
	list := &gentianov1alpha1.OIDCPackCatalogList{}
	if err := c.List(ctx, list); err != nil {
		return Pack{}, nil, false, fmt.Errorf("list OIDCPackCatalog: %w", err)
	}
	for i := range list.Items {
		catalog := &list.Items[i]
		packSpec, ok := catalog.Spec.Packs[clientID]
		if !ok {
			continue
		}
		templates := mapperTemplatesFromCR(catalog.Spec.MapperTemplates)
		pack := packFromCR(packSpec)
		if err := validatePack(clientID, pack, templates); err != nil {
			return Pack{}, nil, false, err
		}
		return pack, templates, true, nil
	}
	return Pack{}, nil, false, nil
}

func packFromCR(spec gentianov1alpha1.OIDCPackSpec) Pack {
	return Pack{
		ScopeName:        spec.ScopeName,
		ScopeDescription: spec.ScopeDescription,
		ClientRole:       spec.ClientRole,
		EntitlementGroup: spec.EntitlementGroup,
		PublicClient:     spec.PublicClient,
		FullScopeAllowed: spec.FullScopeAllowed,
		DefaultScopes:    append([]string(nil), spec.DefaultScopes...),
		Mappers:          append([]string(nil), spec.Mappers...),
	}
}

func mapperTemplatesFromCR(in map[string]gentianov1alpha1.OIDCMapperTemplate) map[string]MapperTemplate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]MapperTemplate, len(in))
	for k, v := range in {
		out[k] = MapperTemplate{
			KeycloakName:   v.KeycloakName,
			ProtocolMapper: v.ProtocolMapper,
			Config:         copyStringMap(v.Config),
		}
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
