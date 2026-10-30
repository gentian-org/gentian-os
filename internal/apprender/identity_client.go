/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// appClient is the app's own OIDC client (requires.services.identity.oidc)
// and everything that hangs off it: its secret, its token mappers, its
// scopes, and the pack's client scope, client role and group grant. A
// profile that declares no client id renders none of it.
func (id *identityScope) appClient() []*unstructured.Unstructured {
	var oidc *gentianov1alpha1.OIDCClientSpec
	if s := id.services(); s.Identity != nil {
		oidc = s.Identity.OIDC
	}
	if oidc == nil || oidc.ClientID == "" {
		return nil
	}
	packKey := oidc.ClientID
	if oidc.OIDCPackRef != "" {
		packKey = oidc.OIDCPackRef
	}
	pack, hasPack := id.packs[packKey]
	accessType := oidc.AccessType
	if accessType == "" {
		accessType = "PUBLIC"
	}
	clientName := id.name("keycloak-client")
	credsName := id.in.Composite + "-oidc-client-creds"

	// ${KERNEL_DOMAIN} as well as the tenant's: a profile sends a person back
	// to the portal after signing out, and Keycloak refused the whole client
	// over the placeholder left in.
	redirects := id.placeAll(oidc.RedirectURIs)
	postLogout := id.placeAll(oidc.PostLogoutRedirectURIs)
	signOutNotice := id.signOutNotice(oidc.BackchannelLogout)
	audiences := identityAudiences(id.in.ProfileName, oidc.Audiences)
	rp := oidc.RequestParameter
	bound := identityRequestBound(rp)
	if bound && id.redirectNarrowed() {
		redirects = append(redirects, "https://*")
	}

	var objs []*unstructured.Unstructured
	if accessType == "CONFIDENTIAL" {
		objs = append(objs, id.clientCreds(credsName,
			fmt.Sprintf("gentian-os/tenants/%s/apps/%s/oidc", id.tenant, id.in.App)))
	}

	// The platform's mark: a realm import makes no client carrying it from a
	// bundle (internal/keycloak PlatformMarkerAttribute). The request
	// parameter is for the platform's executor and mapper, which read it
	// here and nowhere else; the flag is what the realm's client policy
	// matches.
	extraConfig := map[string]interface{}{"gentianos.io/managed-by": "platform"}
	if bound {
		extraConfig["gentianos.io/request-bound-redirect"] = "true"
		extraConfig["gentianos.io/request-parameter"] = rp.Name
		extraConfig["gentianos.io/request-parameter-pattern"] = rp.Pattern
		extraConfig["gentianos.io/request-parameter-redirect-uri"] = rp.RedirectURI
	}
	name := oidc.Name
	if name == "" {
		name = oidc.ClientID
	}
	fp := map[string]interface{}{
		"clientId":                  oidc.ClientID,
		"name":                      name,
		"realmId":                   id.realm,
		"extraConfig":               extraConfig,
		"accessType":                accessType,
		"standardFlowEnabled":       true,
		"directAccessGrantsEnabled": oidc.DirectAccessGrantsEnabled,
		"serviceAccountsEnabled":    false,
		"webOrigins":                []interface{}{"+"},
	}
	// A public client has no secret, so the code is all that stands between
	// whoever receives it and a token: PKCE with S256, on every one.
	if accessType == "PUBLIC" {
		fp["pkceCodeChallengeMethod"] = "S256"
	}
	// No refresh token for a client whose tokens carry the request
	// parameter: a refresh is not an exchange of a code, so its token would
	// not, and the client signs in again instead.
	if bound {
		fp["useRefreshTokens"] = false
	}
	if hasPack {
		fp["fullScopeAllowed"] = pack.FullScopeAllowed
	}
	if len(redirects) > 0 {
		fp["validRedirectUris"] = identityStringList(redirects)
	}
	if len(postLogout) > 0 {
		fp["validPostLogoutRedirectUris"] = identityStringList(postLogout)
	}
	if signOutNotice != "" {
		fp["backchannelLogoutUrl"] = signOutNotice
		fp["backchannelLogoutSessionRequired"] = true
	}
	if accessType == "CONFIDENTIAL" {
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

	// Each name a service the app hands its tokens to has to find in their
	// aud, and nothing in the realm puts it there unless a mapper here does.
	for _, aud := range audiences {
		objs = append(objs, id.audienceMapper(id.in.App, clientName, aud))
	}

	// The request parameter, in the access token. The platform's mapper puts
	// the value in at the exchange of a code only, and only when that
	// exchange's redirect_uri is the address computed from it. Not into the
	// ID token, nor an introspection's answer, which runs the mappers again
	// with no exchange to compare with.
	if bound && rp.Claim != "" {
		objs = append(objs, identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper", id.name("oidc-request-parameter"), map[string]interface{}{
			"deletionPolicy": "Delete",
			"forProvider": map[string]interface{}{
				"realmId":        id.realm,
				"clientIdRef":    identityRef(clientName),
				"name":           "request-parameter-" + rp.Name,
				"protocol":       "openid-connect",
				"protocolMapper": "gentian-request-parameter-mapper",
				"config": map[string]interface{}{
					"claim.name":                rp.Claim,
					"jsonType.label":            "String",
					"id.token.claim":            "false",
					"access.token.claim":        "true",
					"introspection.token.claim": "false",
					"userinfo.token.claim":      "false",
				},
			},
			"providerConfigRef": identityProvider,
		}))
	}

	if hasPack {
		scopes := append([]string{}, pack.DefaultScopes...)
		if pack.ScopeName != "" {
			scopes = append(scopes, pack.ScopeName)
		}
		objs = append(objs, identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "ClientDefaultScopes", id.name("keycloak-default-scopes"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":       id.realm,
				"clientIdRef":   identityRef(clientName),
				"defaultScopes": identityStringList(scopes),
			},
			"providerConfigRef": identityProvider,
		}))
	}

	if o := id.optionalScopes(clientName); o != nil {
		objs = append(objs, o)
	}
	if hasPack {
		objs = append(objs, id.packObjects(pack, clientName)...)
	}
	return objs
}

// optionalScopes lets the mail server accept this app's sign-in tokens
// (requires.services.mail.imap.tokenSignIn): the realm's client scope
// "mailbox" as an optional scope of the app's client, so its tokens name the
// mail server only when it asked for "mailbox" at that sign-in.
//
// Two things have to say so: the profile declares it, and the operator
// answers that this cluster and tenant serve it (MailboxTokenSignIn).
//
// The list is replaced wholesale on reconcile, so it names the five optional
// scopes Keycloak gives every client as well; naming only "mailbox" would
// take offline_access from the client. An app that stops declaring keeps
// this object, without "mailbox": that is what takes the scope off. Dropping
// the object would not -- it is Orphan, because deleting it detaches the
// scopes from a client an uninstall has already removed, and that fails. So
// it is rendered for as long as it has been observed.
func (id *identityScope) optionalScopes(clientName string) *unstructured.Unstructured {
	name := id.name("keycloak-optional-scopes")
	declared := false
	if mail := id.services().Mail; mail != nil && mail.IMAP != nil {
		declared = mail.IMAP.TokenSignIn
	}
	served := declared && id.in.MailboxTokenSignIn
	if !served && id.in.observed(name) == nil {
		return nil
	}
	scopes := []interface{}{"address", "microprofile-jwt", "offline_access", "organization", "phone"}
	if served {
		scopes = append(scopes, "mailbox")
	}
	return identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "ClientOptionalScopes", name, map[string]interface{}{
		"deletionPolicy": "Orphan",
		"forProvider": map[string]interface{}{
			"realmId":        id.realm,
			"clientIdRef":    identityRef(clientName),
			"optionalScopes": scopes,
		},
		"providerConfigRef": identityProvider,
	})
}

// packObjects are what the profile's OIDC pack describes beside the client:
// its client scope and that scope's mappers, the client role, and the
// entitlement group's grant of it.
func (id *identityScope) packObjects(pack gentianov1alpha1.OIDCPackSpec, clientName string) []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	// The client scope is observed, not managed, for now: the OIDC pack Job
	// still creates it, and this proves the provider resolves it from the
	// name the pack gives. A serviceClient pack has none of this.
	if pack.ScopeName != "" && !pack.ServiceClient {
		scopeName := id.name("keycloak-client-scope")
		scope := identityObject("openidclient.keycloak.crossplane.io/v1alpha1", "ClientScope", scopeName, map[string]interface{}{
			"deletionPolicy":     "Orphan",
			"managementPolicies": []interface{}{"Observe"},
			"forProvider": map[string]interface{}{
				"realmId":     id.realm,
				"name":        pack.ScopeName,
				"description": pack.ScopeDescription,
			},
			"providerConfigRef": identityProvider,
		})
		scope.SetAnnotations(map[string]string{"crossplane.io/external-name": pack.ScopeName})
		objs = append(objs, scope)

		// The mappers render only once the scope has been observed and its
		// Keycloak id is known: a mapper's path at the realm needs the
		// scope's UUID, and a reference would resolve to the external-name,
		// which is the scope's name (the create then 404s).
		var scopeID string
		if observed := id.in.observed(scopeName); observed != nil {
			scopeID, _, _ = unstructured.NestedString(observed.Object, "status", "atProvider", "id")
		}
		for _, mapper := range pack.Mappers {
			tmpl, ok := id.mapperTemplates[mapper]
			if !ok || tmpl.ProtocolMapper == "" || scopeID == "" {
				continue
			}
			objs = append(objs, id.scopeMapper(mapper, tmpl, scopeID))
		}
	}

	// The client role the entitlement group is granted. No external-name: a
	// role is identified by its UUID, and the provider finds the existing
	// one from the client and name it is given.
	if pack.ClientRole != "" {
		roleName := id.name("client-role")
		objs = append(objs, identityObject("role.keycloak.crossplane.io/v1alpha1", "Role", roleName, map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":     id.realm,
				"clientIdRef": identityRef(clientName),
				"name":        pack.ClientRole,
			},
			"providerConfigRef": identityProvider,
		}))
		// exhaustive: false, matching the Job, which adds the mapping and
		// leaves anything else on the group alone.
		objs = append(objs, identityObject("group.keycloak.crossplane.io/v1alpha1", "Roles", id.name("group-roles"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":    id.realm,
				"exhaustive": false,
				"groupIdRef": map[string]interface{}{
					"name": identityGroupName(fmt.Sprintf("gentian:tenant:%s:app:%s", id.tenant, id.in.App)),
				},
				"roleIdsRefs": []interface{}{map[string]interface{}{"name": roleName}},
			},
			"providerConfigRef": identityProvider,
		}))
	}
	return objs
}

// scopeMapper is one of the pack's protocol mappers on its client scope. No
// external-name: a mapper is identified by its UUID, and adopting one by
// name fails, so it is created and its UUID recorded. While the pack Job
// still makes the same mapper, the create fails on Keycloak's duplicate
// check, which is the safe way round.
func (id *identityScope) scopeMapper(mapper string, tmpl gentianov1alpha1.OIDCMapperTemplate, scopeID string) *unstructured.Unstructured {
	// keycloakName names the mapper in Keycloak where the catalog's name for
	// it differs ("full name" against full_name); the slug only has to be a
	// valid object name.
	kcName := tmpl.KeycloakName
	if kcName == "" {
		kcName = mapper
	}
	slug := strings.NewReplacer("_", "-", " ", "-", ".", "-").Replace(strings.ToLower(mapper))
	cfg := map[string]interface{}{}
	for k, v := range tmpl.Config {
		cfg[k] = v
	}
	// multivalued defaults in for attribute mappers as the pack Job sets it,
	// or each would strip what the other set on every pass. And
	// introspection.token.claim, which Keycloak writes into every mapper it
	// stores: a config without it never matches what comes back, and the
	// provider and Keycloak loop on it. Both defaulted, never forced.
	if _, ok := cfg["multivalued"]; !ok && tmpl.ProtocolMapper == "oidc-usermodel-attribute-mapper" {
		cfg["multivalued"] = "false"
	}
	if _, ok := cfg["introspection.token.claim"]; !ok {
		cfg["introspection.token.claim"] = "true"
	}
	return identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper", id.name("scope-mapper-"+slug), map[string]interface{}{
		"deletionPolicy": "Orphan",
		"forProvider": map[string]interface{}{
			"realmId":        id.realm,
			"name":           kcName,
			"protocol":       "openid-connect",
			"protocolMapper": tmpl.ProtocolMapper,
			"clientScopeId":  scopeID,
			"config":         cfg,
		},
		"providerConfigRef": identityProvider,
	})
}
