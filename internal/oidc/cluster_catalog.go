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
	"errors"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ErrCatalogAmbiguous is returned by ResolvePack when more than one
// OIDCPackCatalog carries a profile's label: the app Composition reads one of
// them, and which is not defined.
var ErrCatalogAmbiguous = errors.New("more than one OIDCPackCatalog carries the profile's label")

// ResolvePack returns the OIDC pack for clientID from the OIDCPackCatalog
// labelled with profileName (gentianos.io/profile-name), the catalogue the
// profile's bundle brings.
//
// The rule is the app Composition's: it reads that one catalogue, and
// composes the client itself where the catalogue holds no pack for it. Looked
// up anywhere else, a pack in some other catalogue would make the client a
// pack Job's as well as the Composition's. An extension's client is looked up
// in its parent profile's catalogue, where the Composition looks for it.
func ResolvePack(ctx context.Context, c client.Reader, profileName, clientID string) (Pack, map[string]MapperTemplate, bool, error) {
	if clientID == "" || profileName == "" {
		return Pack{}, nil, false, nil
	}
	if c == nil {
		return Pack{}, nil, false, fmt.Errorf("kubernetes client is required")
	}
	list := &gentianov1alpha1.OIDCPackCatalogList{}
	if err := c.List(ctx, list, client.MatchingLabels{gentianov1alpha1.LabelProfileName: profileName}); err != nil {
		return Pack{}, nil, false, fmt.Errorf("list OIDCPackCatalog: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return Pack{}, nil, false, nil
	case 1:
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		sort.Strings(names)
		return Pack{}, nil, false, fmt.Errorf("%w: profile %s, catalogues %v", ErrCatalogAmbiguous, profileName, names)
	}
	catalog := &list.Items[0]
	packSpec, ok := catalog.Spec.Packs[clientID]
	if !ok {
		return Pack{}, nil, false, nil
	}
	templates := mapperTemplatesFromCR(catalog.Spec.MapperTemplates)
	pack := packFromCR(packSpec)
	if err := validatePack(clientID, pack, templates); err != nil {
		return Pack{}, nil, false, err
	}
	return pack, templates, true, nil
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
