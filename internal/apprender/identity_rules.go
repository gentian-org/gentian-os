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
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What a client puts in its tokens is held here to what the definition holds
// it to, so that nothing rests on that alone; the operator holds what needs
// the cluster's other profiles (internal/controller/token_audiences.go). The
// three lists are that file's, and have to stay equal to them. A declaration
// that fails is not rendered at all.

// identityReservedAudiences are names no profile's tokens may carry.
var identityReservedAudiences = []string{
	"account", "account-console", "admin-cli", "broker", "realm-management",
	"security-admin-console", "director", "keycloak", "openbao", "vault",
	"dovecot", "infra-mariadb", "infra-redis", "infra-seaweedfs",
	"keycloak-config", "keycloak-idp", "llm", "mail-edge", "openbao-config",
	"openfga", "postfix",
}

// identityProtocolParameters are the authorization-request parameters of
// OAuth, OpenID Connect and Keycloak, which no request parameter may be.
var identityProtocolParameters = []string{
	"acr_values", "authorization_details", "claims", "client_id",
	"code_challenge", "code_challenge_method", "display", "dpop_jkt",
	"id_token_hint", "kc_action", "kc_idp_hint", "login_hint", "max_age",
	"nonce", "prompt", "redirect_uri", "registration", "request",
	"request_uri", "resource", "response_mode", "response_type", "scope",
	"state", "ui_locales",
}

// identityReservedClaims are claims Keycloak or JWT already gives a meaning.
var identityReservedClaims = []string{
	"acr", "address", "allowed-origins", "amr", "at_hash", "aud", "auth_time",
	"azp", "c_hash", "client_id", "cnf", "email", "email_verified", "exp",
	"family_name", "given_name", "groups", "iat", "iss", "jti", "name", "nbf",
	"nonce", "phone_number", "preferred_username", "realm_access",
	"resource_access", "scope", "sid", "session_state", "sub", "typ",
}

var (
	identityDNSLabel      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	identityPlainPath     = regexp.MustCompile(`^(/[A-Za-z0-9_~-]+(\.[A-Za-z0-9_~-]+)*)+/?$`)
	identityParameterName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	identityBoundRedirect = regexp.MustCompile(`^https://[^*?#\s]*\$\{[^*?#\s]+$`)
)

// identityAudiences keeps the audiences a client may carry: the profile's
// own name, a dash and a label; none beginning with "gentian"; none of the
// names the platform keeps. That no other profile is called so is the
// operator's to say.
func identityAudiences(profileName string, declared []gentianov1alpha1.TokenAudience) []string {
	var out []string
	for _, a := range declared {
		aud := string(a)
		if profileName != "" &&
			strings.HasPrefix(aud, profileName+"-") &&
			len(aud) > len(profileName)+1 &&
			identityDNSLabel.MatchString(aud) &&
			!strings.HasPrefix(aud, "gentian") &&
			!slices.Contains(identityReservedAudiences, aud) {
			out = append(out, aud)
		}
	}
	return out
}

// identityRequestBound says whether a request parameter is held to what the
// definition holds it to: the parameter and its address. The pattern is the
// realm extension's to match, which refuses one it cannot.
func identityRequestBound(rp *gentianov1alpha1.OIDCRequestParameterSpec) bool {
	if rp == nil {
		return false
	}
	return identityParameterName.MatchString(rp.Name) &&
		!slices.Contains(identityProtocolParameters, rp.Name) &&
		rp.Pattern != "" && len(rp.Pattern) <= 512 &&
		len(rp.RedirectURI) <= 512 &&
		identityBoundRedirect.MatchString(rp.RedirectURI) &&
		(rp.Claim == "" || (identityParameterName.MatchString(rp.Claim) &&
			!slices.Contains(identityReservedClaims, rp.Claim)))
}

// identityRedirectNarrowed says whether the realm has the rule that narrows
// https://* again: tenant-default's client policy, Ready. Until it is, a
// request-bound client is registered without https://* and every request
// computed from the parameter is refused as an unknown address.
func (id *identityScope) redirectNarrowed() bool {
	p := id.in.RedirectPolicy
	if p == nil {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
	for _, c := range conditions {
		m, _ := c.(map[string]interface{})
		if m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// signOutNotice is where the realm tells the app that a person signed out
// (backchannelLogout): the app's own Service inside the cluster, never its
// public address, where every path is behind a session the realm does not
// have.
//
// A profile states no address, because the realm sends a request to
// whatever is registered here, so what is registered is only ever the
// Service this component's own entry routes to. Each part is held to what it
// has to be: the entry is one of this profile's and routes to this
// component's own Service; the Service's name and the namespace are DNS
// labels, so nothing can end the host or begin a path; the path is plain (no
// query, no fragment, no "..", no "//"). Anything else registers nothing,
// and the app's session then lasts as long as the app keeps it. The old
// free-form backchannelLogoutUrl is not read.
func (id *identityScope) signOutNotice(bcl *gentianov1alpha1.BackchannelLogoutSpec) string {
	if bcl == nil {
		return ""
	}
	var backend *gentianov1alpha1.BackendRef
	if bcl.Exposure != "" {
		for i := range id.profile.Expose {
			e := &id.profile.Expose[i]
			if e.Name == bcl.Exposure && e.Backend.Component == "" {
				backend = &e.Backend
			}
		}
	}
	if backend == nil {
		return ""
	}
	ns, path, port := id.in.Namespace, bcl.Path, backend.Port
	if !identityDNSLabel.MatchString(backend.Service) || !identityDNSLabel.MatchString(ns) ||
		port <= 0 || port > 65535 || len(path) > 256 || !identityPlainPath.MatchString(path) ||
		strings.Contains(path, "..") || strings.Contains(path, "//") {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", backend.Service, ns, port, path)
}
