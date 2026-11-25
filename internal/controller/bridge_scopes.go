/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app behind a bridge verifies the token it is handed as it verifies its
// own: by its sign-in client as the audience, and by the claims it finds a
// person by. So the scope a token is exchanged for at the door
// (BridgeTokenScope) says exactly that, read from what the app already
// declares for its own sign-in, and nothing a profile could add for the
// occasion:
//
//   - the audience is the client of requires.services.identity.oidc;
//   - the claims are the mappers of that client's OIDC pack, as the pack's
//     own scope carries them.
//
// An app with no sign-in client has nothing to verify such a token with and
// gets no scope: the exchange for it is refused, and its bridge answers
// nobody.

// bridgeScopesDoc is the document under zoneBridgeScopesKey.
type bridgeScopesDoc struct {
	Scopes []bridgeScope `json:"scopes"`
}

// bridgeScope is the scope of one bridged app.
type bridgeScope struct {
	// Profile is the app's profile: the scope is BridgeTokenScope(Profile).
	Profile string `json:"profile"`
	// Audience is the app's own sign-in client.
	Audience string `json:"audience"`
	// Mappers are the claims the app's sign-in tokens carry of a person.
	Mappers []bridgeScopeMapper `json:"mappers"`
}

// bridgeScopeMapper is one protocol mapper of a bridge scope.
type bridgeScopeMapper struct {
	// Name is the mapper's name in the realm, and Slug the same as part of
	// an object's name.
	Name string `json:"name"`
	Slug string `json:"slug"`
	// Type is Keycloak's protocol mapper.
	Type string `json:"type"`
	// Config is the mapper's own configuration. Which tokens a claim is put
	// into is not in it: the realm puts a bridge scope's claims into the
	// access token alone.
	Config map[string]string `json:"config"`
}

// bridgeMapperTokenKeys say which tokens a claim goes into. They are the
// pack's for the app's own sign-in, and are not carried over.
var bridgeMapperTokenKeys = map[string]bool{
	"id.token.claim": true, "access.token.claim": true, "userinfo.token.claim": true,
	"introspection.token.claim": true, "lightweight.claim": true,
}

var bridgeSlugOther = regexp.MustCompile(`[^a-z0-9]+`)

// bridgeSlug is a name as part of a Kubernetes object's name.
func bridgeSlug(name string) string {
	return strings.Trim(bridgeSlugOther.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// bridgeScopeFor is the scope of an app, from its profile and its OIDC pack
// catalog, or false for an app that has no sign-in client.
func bridgeScopeFor(profile *gentianov1alpha1.ComponentProfile, packs *gentianov1alpha1.OIDCPackCatalogSpec) (bridgeScope, bool) {
	services := profile.Services()
	if services == nil || services.Identity == nil || services.Identity.OIDC == nil || services.Identity.OIDC.ClientID == "" {
		return bridgeScope{}, false
	}
	clientID := services.Identity.OIDC.ClientID
	scope := bridgeScope{Profile: profile.Name, Audience: clientID, Mappers: []bridgeScopeMapper{}}
	if packs == nil {
		return scope, true
	}
	seen := map[string]bool{}
	for _, name := range packs.Packs[clientID].Mappers {
		tmpl, ok := packs.MapperTemplates[name]
		slug := bridgeSlug(name)
		if !ok || tmpl.ProtocolMapper == "" || slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		config := map[string]string{}
		for k, v := range tmpl.Config {
			if !bridgeMapperTokenKeys[k] {
				config[k] = v
			}
		}
		mapperName := tmpl.KeycloakName
		if mapperName == "" {
			mapperName = name
		}
		scope.Mappers = append(scope.Mappers, bridgeScopeMapper{Name: mapperName, Slug: slug, Type: tmpl.ProtocolMapper, Config: config})
	}
	sort.Slice(scope.Mappers, func(i, j int) bool { return scope.Mappers[i].Slug < scope.Mappers[j].Slug })
	return scope, true
}

// bridgeScopes is the document for a tenant's bridged apps, by profile name
// and sorted, or "" when none of them has a scope.
func (r *TileProjectionReconciler) bridgeScopes(ctx context.Context, apps []string) (string, error) {
	doc := bridgeScopesDoc{}
	for _, name := range apps {
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		catalogs := &gentianov1alpha1.OIDCPackCatalogList{}
		if err := r.List(ctx, catalogs, client.MatchingLabels{"gentianos.io/profile-name": name}); err != nil && !apimeta.IsNoMatchError(err) {
			return "", err
		}
		var packs *gentianov1alpha1.OIDCPackCatalogSpec
		if len(catalogs.Items) > 0 {
			packs = &catalogs.Items[0].Spec
		}
		if scope, ok := bridgeScopeFor(profile, packs); ok {
			doc.Scopes = append(doc.Scopes, scope)
		}
	}
	if len(doc.Scopes) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
