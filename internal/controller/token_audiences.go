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
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What an app's own sign-in client puts in its tokens beyond what Keycloak
// does: requires.services.identity.oidc.audiences and .requestParameter. The
// app Composition composes both onto the client; this is the operator's half
// of the rules they are held to, the half that needs to know more than the
// one profile. The Composition holds the rest itself, and
// TestTheCompositionHoldsTheSameNames keeps the two lists below equal.
//
// An audience is a grant. A service that checks its name in aud accepts every
// token that carries it, whichever client it was issued to, so a name given to
// one app's client is the key to whatever that service holds. A profile may
// therefore name only audiences of its own: its name, a dash and a label (the
// definition holds that). And none of those may be what something else is
// called -- a service of the platform's, a client Keycloak makes in every
// realm, or another profile, whose name is the audience of the token an
// exchange gives its app (tenant-default, the app scopes).

// reasonSignInClientRefused is the condition reason of an install held by the
// rules here.
const reasonSignInClientRefused = "SignInClientRefused"

// reservedAudiences are names no profile's tokens may carry: the clients
// Keycloak makes in every realm, the platform's services that check an
// audience, and every kernel service (kernel/services). The prefix "gentian"
// is refused besides.
var reservedAudiences = []string{
	"account", "account-console", "admin-cli", "broker", "realm-management", "security-admin-console",
	"director", "keycloak", "openbao", "vault",
	"dovecot", "infra-mariadb", "infra-redis", "infra-seaweedfs", "keycloak-config", "keycloak-idp",
	"llm", "mail-edge", "openbao-config", "openfga", "postfix",
}

// protocolParameters are the authorization-request parameters of OAuth, OpenID
// Connect and Keycloak: none is an app's to bind, and Keycloak keeps none of
// them where the request-parameter mapper reads.
var protocolParameters = []string{
	"acr_values", "authorization_details", "claims", "client_id", "code_challenge", "code_challenge_method",
	"display", "dpop_jkt", "id_token_hint", "kc_action", "kc_idp_hint", "login_hint", "max_age", "nonce",
	"prompt", "redirect_uri", "registration", "request", "request_uri", "resource", "response_mode",
	"response_type", "scope", "state", "ui_locales",
}

// reservedClaims are claims Keycloak or JWT already gives a meaning.
var reservedClaims = []string{
	"acr", "address", "allowed-origins", "amr", "at_hash", "aud", "auth_time", "azp", "c_hash", "client_id",
	"cnf", "email", "email_verified", "exp", "family_name", "given_name", "groups", "iat", "iss", "jti",
	"name", "nbf", "nonce", "phone_number", "preferred_username", "realm_access", "resource_access",
	"scope", "sid", "session_state", "sub", "typ",
}

// redirectGroup is a ${group} of a request parameter's redirect address.
var redirectGroup = regexp.MustCompile(`\$\{([A-Za-z][A-Za-z0-9]*)\}`)

// signInClientRefusal answers why the profile's sign-in client may not be
// composed as declared, or "" when it may. others are the cluster's other
// profiles.
func signInClientRefusal(profile *gentianov1alpha1.ComponentProfile, others []gentianov1alpha1.ComponentProfile, adoptsKernelRealm bool) string {
	refuse := func(format string, args ...any) string {
		return fmt.Sprintf("ComponentProfile %q declares requires.services.identity.oidc.", profile.Name) + fmt.Sprintf(format, args...)
	}
	oidc := declaredOIDCClient(profile)
	// Another profile's audience that is this profile's name would make that
	// profile's tokens the key to this app, whichever came first.
	for i := range others {
		o := &others[i]
		if o.Name == profile.Name {
			continue
		}
		if c := declaredOIDCClient(o); c != nil && slices.Contains(c.Audiences, gentianov1alpha1.TokenAudience(profile.Name)) {
			return fmt.Sprintf("ComponentProfile %q is named %q, which ComponentProfile %q names as an audience of its tokens: those would be accepted as tokens for this app", profile.Name, profile.Name, o.Name)
		}
	}
	if oidc == nil {
		return ""
	}
	for _, a := range oidc.Audiences {
		name := string(a)
		if !strings.HasPrefix(name, profile.Name+"-") || len(name) == len(profile.Name)+1 {
			return refuse("audiences names %q: an audience is the profile's own, its name, a dash and a label", name)
		}
		if strings.HasPrefix(name, "gentian") || slices.Contains(reservedAudiences, name) {
			return refuse("audiences names %q, which is the name of a service of the platform's or a client of every realm", name)
		}
		for i := range others {
			if others[i].Name == name {
				return refuse("audiences names %q, which is another profile: its app would accept these tokens", name)
			}
		}
	}
	if p := oidc.RequestParameter; p != nil {
		if problem := requestParameterProblem(p); problem != "" {
			return refuse("requestParameter %s", problem)
		}
		if adoptsKernelRealm {
			return refuse("requestParameter: the tenant signs in in the kernel realm, which does not have the rule that holds a request to its parameter")
		}
	}
	return ""
}

func declaredOIDCClient(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.OIDCClientSpec {
	s := profile.Services()
	if s == nil || s.Identity == nil {
		return nil
	}
	return s.Identity.OIDC
}

// requestParameterProblem is what is wrong with a request parameter, or "".
// The pattern is matched in Keycloak, by Java: it is held to the syntax Go and
// Java share, which is also what keeps it free of back-references.
func requestParameterProblem(p *gentianov1alpha1.OIDCRequestParameterSpec) string {
	if slices.Contains(protocolParameters, p.Name) {
		return fmt.Sprintf("names the parameter %q, which is the protocol's own", p.Name)
	}
	if p.Claim != "" && slices.Contains(reservedClaims, p.Claim) {
		return fmt.Sprintf("names the claim %q, which Keycloak or JWT already gives a meaning", p.Claim)
	}
	if strings.Contains(p.Pattern, "(?P<") {
		return "spells a named group (?P<name>...), which Java does not read: write (?<name>...)"
	}
	re, err := regexp.Compile(`^(?:` + p.Pattern + `)$`)
	if err != nil {
		return fmt.Sprintf("has a pattern that is not one: %v", err)
	}
	groups := re.SubexpNames()
	used := redirectGroup.FindAllStringSubmatch(p.RedirectURI, -1)
	if len(used) == 0 {
		return "has a redirectUri that names no ${group} of the pattern, so it is not computed from the parameter"
	}
	for _, g := range used {
		if !slices.Contains(groups, g[1]) {
			return fmt.Sprintf("has a redirectUri that names ${%s}, which the pattern has no group of", g[1])
		}
	}
	if strings.Contains(redirectGroup.ReplaceAllString(p.RedirectURI, ""), "$") {
		return "has a redirectUri with a $ that is not a ${group}"
	}
	u, err := url.Parse(redirectGroup.ReplaceAllString(p.RedirectURI, "a"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p.RedirectURI, "*?#") {
		return "has a redirectUri that is not an https address without a wildcard, query or fragment"
	}
	return ""
}

// signInClientRefusalFor reads the cluster's profiles and answers
// signInClientRefusal for an install.
func (r *ComponentReconciler) signInClientRefusalFor(ctx context.Context, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (string, error) {
	profiles := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profiles); err != nil {
		return "", err
	}
	return signInClientRefusal(profile, profiles.Items, tenantAdoptsKernelRealm(tenant, r.kernelRealm())), nil
}
