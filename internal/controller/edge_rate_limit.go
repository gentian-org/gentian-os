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

// Rate limits at the Gateway.
//
// Two places a credential is posted to with no session in front -- the
// identity provider's sign-in pages, and a sign-in sidecar's answer path --
// hold each client address to a number of posts a minute. Every route behind
// a session holds each client address to a number of requests a minute.
//
// They are Envoy's LOCAL rate limit (BackendTrafficPolicy.rateLimit.local):
// a token bucket in each Envoy pod, with no service beside it to install or
// to fail. Local means per pod, so a client address is allowed the number
// below from EACH proxy replica; a limit is a ceiling against one address
// sending as fast as it can, not a count anybody can rely on to the request.
// The identity provider's own brute-force detection, which counts failures
// per account, is what stops slow guessing and is untouched.
//
// One bucket per client address ("Distinct"). Which address that is depends
// on how the cluster is reached (edgeClientAddressHeader): the address the
// Gateway saw the connection come from, or, behind the tunnel, the one the
// tunnel names. A request that carries no such header behind the tunnel did
// not come through it -- it is a pod of this cluster calling the public name
// -- and is not counted.
//
// Only what a person's browser posts is limited at sign-in. The token
// endpoint is not: the Gateway's own code exchange and every app's
// server-to-server calls arrive there from a handful of addresses that stand
// for everybody.
//
// Behind a session the count is of requests, whatever their size, over a
// minute: a page that loads a hundred assets at once, a sync client walking
// a folder tree and an upload sent in chunks all fit in the allowance, and
// each route has its own bucket, so one app's traffic does not use up
// another's.

// defaultEdgeSignInPostsPerMinute is how many sign-in posts one client
// address may make a minute, at each Envoy pod.
const defaultEdgeSignInPostsPerMinute = 60

// defaultEdgeSessionRequestsPerMinute is how many requests one client
// address may make a minute to one route behind a session, at each Envoy
// pod: five hundred a second on average, and the whole minute's worth at
// once. It sits above what one person's clients make, so it holds back a
// single runaway client and nobody else.
const defaultEdgeSessionRequestsPerMinute = 30000

// edgeSignInPostsPerMinute is the sign-in limit in force. The cluster's
// administrator may set it through the operator's environment
// (EDGE_SIGN_IN_POSTS_PER_MINUTE); 0 turns the limit off.
func edgeSignInPostsPerMinute() int {
	return edgeLimitFromEnv("EDGE_SIGN_IN_POSTS_PER_MINUTE", defaultEdgeSignInPostsPerMinute)
}

// edgeSessionRequestsPerMinute is the limit in force behind a session. The
// cluster's administrator may set it through the operator's environment
// (EDGE_SESSION_REQUESTS_PER_MINUTE); 0 turns the limit off.
func edgeSessionRequestsPerMinute() int {
	return edgeLimitFromEnv("EDGE_SESSION_REQUESTS_PER_MINUTE", defaultEdgeSessionRequestsPerMinute)
}

// edgeLimitFromEnv is the number the environment names under key, or def
// where it names none or one that is not a count.
func edgeLimitFromEnv(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 1000000 {
		return def
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
	return edgeLocalRateLimit(edgeSignInPostsPerMinute(), "POST", pathPattern, clientAddressHeader)
}

// edgeSessionRateLimit is the rateLimit block of a BackendTrafficPolicy that
// holds every request to a route behind a session to one client address's
// allowance, or nil when the limit is off.
func edgeSessionRateLimit(clientAddressHeader string) map[string]interface{} {
	return edgeLocalRateLimit(edgeSessionRequestsPerMinute(), "", "", clientAddressHeader)
}

// edgeLocalRateLimit holds each client address to perMinute requests, nil
// for 0. method and pathPattern narrow what is counted; empty counts all.
func edgeLocalRateLimit(perMinute int, method, pathPattern, clientAddressHeader string) map[string]interface{} {
	if perMinute == 0 {
		return nil
	}
	selector := func(client map[string]interface{}) map[string]interface{} {
		s := map[string]interface{}{}
		if method != "" {
			s["methods"] = []interface{}{map[string]interface{}{"value": method}}
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
