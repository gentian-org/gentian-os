/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import "strings"

// HeaderCookie is the request header the session's cookies are removed from.
const HeaderCookie = "cookie"

// ownsCookie reports whether a cookie of that name is the edge's on this
// route: one of the names the route's policy gives the session's tokens, or
// one the Gateway's OAuth2 filter names itself, which is a fixed word and a
// suffix the filter chooses. Names are compared as written; a cookie name is
// case-sensitive.
func (r *Route) ownsCookie(name string) bool {
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
