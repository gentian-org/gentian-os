/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package oidc resolves OIDC client packs from cluster-scoped OIDCPackCatalog CRs.
// App-specific mapper templates live in the catalogue repositories, not as
// hardcoded constants in the operator.
package oidc

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResolvePack returns the OIDC pack for clientID from cluster OIDCPackCatalog CRs.
//
// The rule: every OIDCPackCatalog on the cluster is listed, and the first one
// that holds a pack under clientID is taken. Nothing else about the catalog is
// asked -- not its name, not its labels, not whether a profile's bundle still
// brings it. A catalog that is the only one holding a client id is therefore
// always the one used for it, and where two hold the same id, which is used
// is the order the list came in (PackHolders).
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

// PackHolders answers, for every client id any OIDCPackCatalog on the cluster
// holds a pack for, the names of the catalogs that hold one, sorted. It is
// ResolvePack's rule read the other way round: a client id with one holder
// resolves to that catalog, and one with several resolves to whichever of
// them the list returns first.
func PackHolders(ctx context.Context, c client.Reader) (map[string][]string, error) {
	list := &gentianov1alpha1.OIDCPackCatalogList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list OIDCPackCatalog: %w", err)
	}
	out := map[string][]string{}
	for i := range list.Items {
		for clientID := range list.Items[i].Spec.Packs {
			out[clientID] = append(out[clientID], list.Items[i].Name)
		}
	}
	for clientID := range out {
		sort.Strings(out[clientID])
	}
	return out, nil
}

func packFromCR(spec gentianov1alpha1.OIDCPackSpec) Pack {
	return Pack{
		ServiceClient:    spec.ServiceClient,
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
