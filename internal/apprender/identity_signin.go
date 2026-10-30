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
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// signInSidecar is the sign-in sidecar's registration at the realm
// (requires.services.identity.sidecar), for an app that can do neither OIDC
// nor SAML itself: a SAML service provider beside the app runs the app's
// handler, which makes a session for the person the realm vouches for. The
// sidecar itself, its settings, network paths and routes are the operator's
// (internal/controller/signin_sidecar.go).
//
// Rendered only where the profile declares the sidecar and the operator says
// where the app answers (SignInSidecar's host), which it does only for an
// install whose bundle brings the handler. Either alone renders nothing, and
// a profile that stops declaring it loses the client: it is deleted from the
// realm. Nothing here is the profile's to choose.
func (id *identityScope) signInSidecar() []*unstructured.Unstructured {
	identity := id.services().Identity
	if identity == nil || identity.Sidecar == nil {
		return nil
	}
	host, _ := id.in.SignInSidecar["host"].(string)
	if host == "" {
		return nil
	}
	app, ns := id.in.App, id.in.Namespace
	entity := fmt.Sprintf("https://%s/sso", host)
	acs := fmt.Sprintf("https://%s/sso/acs", host)
	clientName := id.name("sign-in-client")

	// Where the realm tells the sidecar that a person signed out: its own
	// Service, which the operator names <app>-sign-in in this namespace. The
	// operator says the port and nothing else of the address, so the realm
	// calls nothing but this app's sidecar; no port, and it calls nothing.
	logout := ""
	if port := identityInt(id.in.SignInSidecar["logoutPort"]); port > 0 && port <= 65535 &&
		identityDNSLabel.MatchString(app+"-sign-in") && identityDNSLabel.MatchString(ns) {
		logout = fmt.Sprintf("http://%s-sign-in.%s.svc.cluster.local:%d/sso/logout", app, ns, port)
	}

	fp := map[string]interface{}{
		"realmId":     id.realm,
		"clientId":    entity,
		"name":        "Sign-in sidecar of " + app,
		"enabled":     true,
		"extraConfig": map[string]interface{}{"gentianos.io/managed-by": "platform"},
		// The response and its assertion are both signed by the realm; the
		// sidecar accepts nothing less.
		"signDocuments":      true,
		"signAssertions":     true,
		"signatureAlgorithm": "RSA_SHA256",
		"encryptAssertions":  false,
		// The sidecar holds no key, so its requests are unsigned: the answer
		// goes to the one address below whoever asked, and the sidecar
		// accepts an answer only to a request it sent itself.
		"clientSignatureRequired": false,
		"forcePostBinding":        true,
		// The person is named by e-mail address, whatever a request asks
		// for: it is the one thing the sidecar reads.
		"forceNameIdFormat":     true,
		"nameIdFormat":          "email",
		"includeAuthnStatement": true,
		// Sign-out is server to server, a signed LogoutRequest to the
		// sidecar itself; the browser is asked for nothing.
		"frontChannelLogout":       false,
		"fullScopeAllowed":         false,
		"validRedirectUris":        []interface{}{acs},
		"assertionConsumerPostUrl": acs,
	}
	if logout != "" {
		fp["logoutServicePostBindingUrl"] = logout
	}
	objs := []*unstructured.Unstructured{
		identityObject("samlclient.keycloak.crossplane.io/v1alpha1", "Client", clientName, map[string]interface{}{
			"deletionPolicy":     "Delete",
			"managementPolicies": identityManaged,
			"forProvider":        fp,
			"providerConfigRef":  identityProvider,
		}),
	}

	// Who administers the app. An app signed in to this way has no claim of
	// its own to read, so the realm tells the sidecar in the signed answer
	// whether the person is in the tenant's app administrators' group
	// (internal/keycloak/groups.go): one role at the sidecar's client, that
	// role granted to the group (exhaustive: false, adding one mapping), and
	// a mapper listing the person's roles at this client as Role -- the name
	// of the list the realm gives every SAML client by default.
	// fullScopeAllowed is false above, so the list is that role or nothing.
	//
	// Orphan on all of them, and nothing is left behind by it: the realm
	// removes a client's roles, their grants and its mappers with the
	// client.
	roleName := id.name("sign-in-admin-role")
	adminsGroup := identityGroupName(fmt.Sprintf("gentian:tenant:%s:app-admins", id.tenant))
	objs = append(objs,
		identityObject("role.keycloak.crossplane.io/v1alpha1", "Role", roleName, map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":         id.realm,
				"samlClientIdRef": identityRef(clientName),
				"name":            "gentian-app-admin",
				"description":     fmt.Sprintf("Administers %s: held by the tenant's app administrators", app),
			},
			"providerConfigRef": identityProvider,
		}),
		identityObject("group.keycloak.crossplane.io/v1alpha1", "Roles", id.name("sign-in-admin-grant"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":     id.realm,
				"exhaustive":  false,
				"groupIdRef":  identityRef(adminsGroup),
				"roleIdsRefs": []interface{}{identityRef(roleName)},
			},
			"providerConfigRef": identityProvider,
		}),
		identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper", id.name("sign-in-admin-mapper"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":         id.realm,
				"samlClientIdRef": identityRef(clientName),
				"name":            "app-admin",
				"protocol":        "saml",
				"protocolMapper":  "saml-role-list-mapper",
				"config": map[string]interface{}{
					"attribute.name":       "Role",
					"attribute.nameformat": "Basic",
					"single":               "true",
				},
			},
			"providerConfigRef": identityProvider,
		}),
		// Which address names the person: the workspace address, which is
		// the username. Without this the realm fills the e-mail NameID from
		// the e-mail field, where a tenant's realm keeps a recovery address,
		// and the sidecar refuses a sign-in whose two addresses differ. The
		// realm applies a NameID mapper only for the format the answer is
		// made in, by its full name: the client forces e-mail.
		identityObject("client.keycloak.crossplane.io/v1alpha1", "ProtocolMapper", id.name("sign-in-name-id-mapper"), map[string]interface{}{
			"deletionPolicy": "Orphan",
			"forProvider": map[string]interface{}{
				"realmId":         id.realm,
				"samlClientIdRef": identityRef(clientName),
				"name":            "name-id",
				"protocol":        "saml",
				"protocolMapper":  "saml-user-attribute-nameid-mapper",
				"config": map[string]interface{}{
					"user.attribute":       "username",
					"mapper.nameid.format": "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
				},
			},
			"providerConfigRef": identityProvider,
		}),
	)
	return objs
}

// identityInt reads a number the reconciler or a decoded claim gives in any
// of its forms; anything else is 0.
func identityInt(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}
