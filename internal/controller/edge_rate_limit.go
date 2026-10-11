/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"os"
	"strconv"
	"strings"
)

// Rate limits at the Gateway, for the two places a credential is posted to
// with no session in front: the identity provider's sign-in pages, and a
// sign-in sidecar's answer path.
//
// They are Envoy's LOCAL rate limit (BackendTrafficPolicy.rateLimit.local):
// a token bucket in each Envoy pod, with no service beside it to install or
// to fail. Local means per pod, so a client address is allowed the number
// below from EACH proxy replica; the limit is a ceiling against one address
// guessing passwords as fast as it can post, not a count anybody can rely on
// to the request. The identity provider's own brute-force detection, which
// counts failures per account, is what stops slow guessing and is untouched.
//
// One bucket per client address ("Distinct"). Which address that is depends
// on how the cluster is reached (edgeClientAddressHeader): the address the
// Gateway saw the connection come from, or, behind the tunnel, the one the
// tunnel names. A request that carries no such header behind the tunnel did
// not come through it -- it is a pod of this cluster calling the public name
// -- and is not counted.
//
// Only what a person's browser posts is limited. The token endpoint is not:
// the Gateway's own code exchange and every app's server-to-server calls
// arrive there from a handful of addresses that stand for everybody.

// defaultEdgeSignInPostsPerMinute is how many sign-in posts one client
// address may make a minute, at each Envoy pod.
const defaultEdgeSignInPostsPerMinute = 60

// edgeSignInPostsPerMinute is the limit in force. The cluster's
// administrator may set it through the operator's environment
// (EDGE_SIGN_IN_POSTS_PER_MINUTE); 0 turns the limit off.
func edgeSignInPostsPerMinute() int {
	raw := strings.TrimSpace(os.Getenv("EDGE_SIGN_IN_POSTS_PER_MINUTE"))
	if raw == "" {
		return defaultEdgeSignInPostsPerMinute
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 1000000 {
		return defaultEdgeSignInPostsPerMinute
	}
	return n
}

// keycloakSignInPostPattern matches what a sign-in page posts a credential
// to, in any realm: the path with its query, which is what Envoy matches.
const keycloakSignInPostPattern = `/auth/realms/[^/]+/login-actions/.*`

// edgeSignInRateLimit is the rateLimit block of a BackendTrafficPolicy that
// holds POSTs to one client address's allowance, or nil when the limit is
// off. pathPattern narrows it to the paths that match; empty is every path
// of the route. clientAddressHeader is edgeClientAddressHeader's answer for
// this cluster.
func edgeSignInRateLimit(pathPattern, clientAddressHeader string) map[string]interface{} {
	perMinute := edgeSignInPostsPerMinute()
	if perMinute == 0 {
		return nil
	}
	selector := func(client map[string]interface{}) map[string]interface{} {
		s := map[string]interface{}{
			"methods": []interface{}{map[string]interface{}{"value": "POST"}},
		}
		if pathPattern != "" {
			s["path"] = map[string]interface{}{"type": "RegularExpression", "value": pathPattern}
		}
		for k, v := range client {
			s[k] = v
		}
		return s
	}
	rule := func(client map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"clientSelectors": []interface{}{selector(client)},
			"limit":           map[string]interface{}{"requests": int64(perMinute), "unit": "Minute"},
		}
	}
	var rules []interface{}
	if header := clientAddressHeader; header != "" {
		rules = append(rules, rule(map[string]interface{}{
			"headers": []interface{}{map[string]interface{}{"name": header, "type": "Distinct"}},
		}))
	} else {
		// Two rules, because a selector names one range: every IPv4 address
		// its own bucket, and every IPv6 address its own.
		for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
			rules = append(rules, rule(map[string]interface{}{
				"sourceCIDR": map[string]interface{}{"type": "Distinct", "value": cidr},
			}))
		}
	}
	return map[string]interface{}{
		"type":  "Local",
		"local": map[string]interface{}{"rules": rules},
	}
}
