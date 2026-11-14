/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"fmt"
	"net/http"
	"strings"
)

// HeaderCookie is the request header the session's cookies are removed from.
const HeaderCookie = "cookie"

// ownsCookie reports whether a cookie of that name is the edge's on this
// route: one of the names the route's policy gives the session's tokens, or
// one the Gateway's OAuth2 filter names itself, which is a fixed word and a
// suffix the filter chooses. Names are compared as written; a cookie name is
// case-sensitive.
func (r *Route) ownsCookie(name string) bool {
	if name == RestartCookie {
		return true
	}
	for _, n := range r.SessionCookies {
		if name == n {
			return true
		}
	}
	for _, p := range r.SessionCookiePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// withoutSessionCookies is a Cookie header with the edge's cookies taken out
// and everything else left as it was: the remaining cookies keep their bytes,
// their order and the separators between them. changed is false when the
// header holds none of the edge's cookies, and then it is not to be touched
// at all.
//
// This is not reading the session. Who is asking was settled from the token
// the Gateway handed over; the cookies are looked at by name only, to keep
// the tokens in them from going on to the backend.
//
// The header is split on ";" and nothing else, which is how the browser built
// it and how the Gateway's filter reads it: a cookie's value cannot hold one,
// quoted or not. A piece with no "=" is not a cookie with a name and is kept.
func (r *Route) withoutSessionCookies(header string) (out string, changed bool) {
	if r == nil || header == "" || (len(r.SessionCookies) == 0 && len(r.SessionCookiePrefixes) == 0) {
		return header, false
	}
	pieces := strings.Split(header, ";")
	kept := pieces[:0:0]
	for _, piece := range pieces {
		if eq := strings.IndexByte(piece, '='); eq >= 0 && r.ownsCookie(strings.Trim(piece[:eq], " \t")) {
			changed = true
			continue
		}
		kept = append(kept, piece)
	}
	if !changed {
		return header, false
	}
	// The space after a separator belonged to the piece that followed it;
	// where the first piece went, the next one's leading space goes too.
	return strings.TrimLeft(strings.Join(kept, ";"), " \t"), true
}

// sessionCookieNames are the names of the edge's cookies in a Cookie header,
// each once, in the order they came, RestartCookie aside.
func (r *Route) sessionCookieNames(header string) []string {
	var names []string
	seen := map[string]bool{}
	for _, piece := range strings.Split(header, ";") {
		eq := strings.IndexByte(piece, '=')
		if eq < 0 {
			continue
		}
		name := strings.Trim(piece[:eq], " \t")
		if name == RestartCookie || seen[name] || !r.ownsCookie(name) {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// hasCookie reports whether a Cookie header holds a cookie of that name.
func hasCookie(header, name string) bool {
	for _, piece := range strings.Split(header, ";") {
		if eq := strings.IndexByte(piece, '='); eq >= 0 && strings.Trim(piece[:eq], " \t") == name {
			return true
		}
	}
	return false
}

// restartSignIn answers a page whose session has ended: the realm ended it
// (a sign-out elsewhere, an administrator, its lifetime), the Gateway's
// filter could not refresh it, and passed the request on with the session's
// cookies and no token. Refused, the person would look at a page saying they
// lack access, when all they lack is a session.
//
// So the cookies that hold the ended session are cleared, and the browser is
// sent back to the address it asked for. Without them the filter starts a
// sign-in, which comes straight back while the realm still has a session for
// the person, and shows the login form when it has none. Nothing is granted:
// the answer reaches no backend, and the next request is judged on whatever
// session the sign-in makes.
//
// Only a page the person opened (a GET the browser calls a navigation), and
// only one that carries the edge's cookies: a fetch cannot follow a redirect
// to sign in and keeps its 401, and a request with none of the cookies has no
// session to have ended. Once per restartWindow on the host (RestartCookie):
// a page that comes back without a session after a fresh sign-in has a fault
// a further round would not mend, and gets the refusal.
func restartSignIn(route *Route, req Request, reason string) (Decision, bool) {
	if route == nil || route.AuthMode != AuthModeOIDC || req.Method != http.MethodGet || req.FetchMode != "navigate" {
		return Decision{}, false
	}
	names := route.sessionCookieNames(req.Cookie)
	if len(names) == 0 || hasCookie(req.Cookie, RestartCookie) {
		return Decision{}, false
	}
	// The filter sets its cookies on the host alone and on every path, so
	// that is what clears them.
	const attrs = "; Path=/; Secure; HttpOnly; SameSite=Lax"
	cookies := make([]string, 0, len(names)+1)
	for _, n := range names {
		cookies = append(cookies, n+"=; Max-Age=0"+attrs)
	}
	cookies = append(cookies, fmt.Sprintf("%s=1; Max-Age=%d%s", RestartCookie, int(restartWindow.Seconds()), attrs))
	return Decision{
		Status: http.StatusFound, Redirect: samePage(req.Path),
		Reason: "session ended: " + reason, SetCookies: cookies, restart: true,
	}, true
}

// samePage is the address to send the browser back to: the path and query it
// asked for on this host. Anything that would leave the host -- "//other" is
// another host to a browser, and so is "/\other" to some -- is the host's
// front page instead.
func samePage(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.HasPrefix(path, "/\\") ||
		strings.ContainsAny(path, "\r\n") {
		return "/"
	}
	return path
}
