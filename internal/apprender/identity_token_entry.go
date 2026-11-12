/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TokenEntry is a component's token entry while it is served: the entry's
// host, and the addresses a program may be sent back to after the person
// signed in. The operator sets it once the entry is approved and leaves it
// nil otherwise, so the realm holds the client below for exactly as long.
type TokenEntry struct {
	Host         string
	RedirectURIs []string
}

// TokenEntryClientID is the realm client the programs that connect to a
// component's token entry sign people in through. One per component, named
// after it, so a person setting a program up can be told what to enter.
func TokenEntryClientID(app string) string { return "token-entry-" + app }

// tokenEntrySessionMaxAge is how long a sign-in through a token entry's
// client may be used before the person signs in again.
const tokenEntrySessionMaxAge = 4 * time.Hour

// tokenEntryAudience is what the entry's tokens are made out to, and what the
// bouncer holds them against: the entry's own address.
func tokenEntryAudience(host string) string { return "https://" + host }

// tokenEntryClient is the client a program signs a person in through to
// reach the component's token entry, and what makes its tokens out to the
// entry and to nothing else.
//
// Public, because a program on a person's computer keeps no secret: PKCE
// with S256 stands in for one. The person is asked to consent, and is shown
// the client's name. No password grant, no service account, no implicit
// flow, and no role of the person is put into the token (fullScopeAllowed
// false): the token says who the person is and names the entry, and what the
// person may do is asked where it is used.
//
// A profile that declares no address to send a program back to renders no
// client: Keycloak would refuse every sign-in, and there is nothing to
// register.
func (id *identityScope) tokenEntryClient() []*unstructured.Unstructured {
	entry := id.in.TokenEntry
	if entry == nil || entry.Host == "" || len(entry.RedirectURIs) == 0 {
		return nil
	}
	clientName := id.name("token-entry-client")
	fp := map[string]interface{}{
		"clientId":                  TokenEntryClientID(id.in.App),
		"name":                      "Programs connecting to " + entry.Host,
		"realmId":                   id.realm,
		"extraConfig":               map[string]interface{}{"gentianos.io/managed-by": "platform"},
		"accessType":                "PUBLIC",
		"pkceCodeChallengeMethod":   "S256",
		"standardFlowEnabled":       true,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"serviceAccountsEnabled":    false,
		"fullScopeAllowed":          false,
		"consentRequired":           true,
		"displayOnConsentScreen":    true,
		"validRedirectUris":         identityStringList(entry.RedirectURIs),
	}
	// A program keeps what renews its tokens on the person's computer, and
	// the realm rotates that for no single client. So the client's sessions
	// end after tokenEntrySessionMaxAge whatever the realm's own limit, or
	// sooner where the profile says so: past it the realm refuses the
	// renewal and the person signs in again.
	max := tokenEntrySessionMaxAge
	if own := id.profile.SessionMaxAge; own != nil && own.Duration > 0 && own.Duration < max {
		max = own.Duration
	}
	fp["clientSessionMaxLifespan"] = max.String()
	objs := []*unstructured.Unstructured{
		identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "Client", clientName, map[string]interface{}{
			"deletionPolicy":     "Delete",
			"managementPolicies": identityManaged,
			"forProvider":        fp,
			"providerConfigRef":  identityProvider,
		}),
		id.tokenEntryAudienceMapper(clientName, "token-entry-audience", "audience-entry", tokenEntryAudience(entry.Host)),
		// The optional scopes Keycloak gives every client, without
		// offline_access: a token of this client ends with the person's
		// session, and none outlives it. Deleted with the client.
		identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "ClientOptionalScopes", id.name("token-entry-optional-scopes"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":        id.realm,
				"clientIdRef":    identityRef(clientName),
				"optionalScopes": []interface{}{"address", "microprofile-jwt", "organization", "phone"},
			},
			"providerConfigRef": identityProvider,
		}),
	}
	// The component behind the entry exchanges the token it is handed for
	// one per app, and the realm lets a client exchange only a token that
	// names it. So the component's own client is named too, where it has one.
	if s := id.services(); s.Identity != nil && s.Identity.OIDC != nil && s.Identity.OIDC.ClientID != "" {
		objs = append(objs, id.tokenEntryAudienceMapper(clientName, "token-entry-audience-own", "audience-component", s.Identity.OIDC.ClientID))
	}
	return objs
}

// tokenEntryAudienceMapper puts one name in the aud of the entry client's
// access tokens, and in nothing else of theirs.
func (id *identityScope) tokenEntryAudienceMapper(client, suffix, name, aud string) *unstructured.Unstructured {
	return identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper", id.name(suffix), map[string]interface{}{
		"deletionPolicy": "Delete",
		"forProvider": map[string]interface{}{
			"realmId":        id.realm,
			"clientIdRef":    identityRef(client),
			"name":           name,
			"protocol":       "openid-connect",
			"protocolMapper": "oidc-audience-mapper",
			"config": map[string]interface{}{
				"included.custom.audience":  aud,
				"id.token.claim":            "false",
				"access.token.claim":        "true",
				"introspection.token.claim": "true",
				"userinfo.token.claim":      "false",
			},
		},
		"providerConfigRef": identityProvider,
	})
}
