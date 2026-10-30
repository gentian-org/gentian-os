/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// extensionClients are the clients of the extensions that sign people in
// themselves (extensions[].kernelRequirements.identity.oidc), each under a
// client of its own. The operator leaves them to this render
// (crossplaneOwnsOIDCClient) and seeds each secret at the extension's key,
// <app>-<extension> (SidecarAppName), so each is rendered in the shape of
// the app's own client: the tenant's domain put into its addresses, PKCE
// with S256 for a public one, the secret from that key for a confidential
// one, and the audiences held as the app's are.
//
// Where an extension declares no redirect address the profile's default
// (gentianos.io/oidc-default-redirect-uris) is used, as the operator does;
// with neither, the operator refuses the client and nothing is rendered.
// Nor is a client the profile's OIDC pack catalog holds a pack for, which
// the pack Job makes, or one that declares requestParameter, which only the
// app's own client may.
func (id *identityScope) extensionClients() []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	for _, ext := range id.profile.Extensions {
		reqs := ext.ServiceRequirements
		if reqs == nil || reqs.Identity == nil || reqs.Identity.OIDC == nil {
			continue
		}
		oidc := reqs.Identity.OIDC
		packKey := oidc.ClientID
		if oidc.OIDCPackRef != "" {
			packKey = oidc.OIDCPackRef
		}
		if _, packed := id.packs[packKey]; ext.Name == "" || oidc.ClientID == "" || oidc.RequestParameter != nil || packed {
			continue
		}
		key := id.in.App + "-" + ext.Name
		declared := oidc.RedirectURIs
		if len(declared) == 0 {
			declared = id.defaultRedirects()
		}
		redirects := id.placeAll(declared)
		if len(redirects) == 0 {
			continue
		}
		postLogout := id.placeAll(oidc.PostLogoutRedirectURIs)
		accessType := oidc.AccessType
		if accessType == "" {
			accessType = "PUBLIC"
		}
		confidential := accessType == "CONFIDENTIAL"
		clientName := id.tenant + "-" + key + "-keycloak-client"
		credsName := id.in.Composite + "-" + ext.Name + "-oidc-client-creds"

		if confidential {
			objs = append(objs, id.clientCreds(credsName,
				fmt.Sprintf("gentian-os/tenants/%s/apps/%s/oidc", id.tenant, key)))
		}
		name := oidc.Name
		if name == "" {
			name = oidc.ClientID
		}
		fp := map[string]interface{}{
			"clientId":                  oidc.ClientID,
			"name":                      name,
			"realmId":                   id.realm,
			"extraConfig":               map[string]interface{}{"gentianos.io/managed-by": "platform"},
			"accessType":                accessType,
			"standardFlowEnabled":       true,
			"directAccessGrantsEnabled": oidc.DirectAccessGrantsEnabled,
			"serviceAccountsEnabled":    false,
			"validRedirectUris":         identityStringList(redirects),
			"webOrigins":                []interface{}{"+"},
		}
		if !confidential {
			fp["pkceCodeChallengeMethod"] = "S256"
		}
		if len(postLogout) > 0 {
			fp["validPostLogoutRedirectUris"] = identityStringList(postLogout)
		}
		if confidential {
			fp["clientSecretSecretRef"] = map[string]interface{}{
				"name":      credsName,
				"namespace": id.provNs,
				"key":       "client-secret",
			}
		}
		objs = append(objs, identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "Client", clientName, map[string]interface{}{
			"deletionPolicy":     "Delete",
			"managementPolicies": identityManaged,
			"forProvider":        fp,
			"providerConfigRef":  identityProvider,
		}))
		for _, aud := range identityAudiences(id.in.ProfileName, oidc.Audiences) {
			objs = append(objs, id.audienceMapper(key, clientName, aud))
		}
	}
	return objs
}

// defaultRedirects are the profile's default redirect addresses for an
// extension's client, from its gentianos.io/oidc-default-redirect-uris
// annotation; anything but a JSON list of strings is none.
func (id *identityScope) defaultRedirects() []string {
	raw := id.in.ProfileAnnotations["gentianos.io/oidc-default-redirect-uris"]
	var list []interface{}
	if raw == "" || json.Unmarshal([]byte(raw), &list) != nil {
		return nil
	}
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
