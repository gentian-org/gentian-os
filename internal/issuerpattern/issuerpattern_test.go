/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package issuerpattern

import "testing"

// issuerAllowedCases is the transcript of Allowed: anchoring, "*",
// case, trailing slash, scheme and the characters a regular expression
// would read as syntax.
var issuerAllowedCases = []struct {
	name     string
	patterns []string
	issuer   string
	want     bool
}{
	{"no patterns", nil, "https://id.a.example/realms/b", false},
	{"empty pattern list", []string{}, "https://id.a.example/realms/b", false},
	{"exact", []string{"https://id.a.example/realms/b"}, "https://id.a.example/realms/b", true},
	{"anchored at the end", []string{"https://id.a.example/realms/b"}, "https://id.a.example/realms/bc", false},
	{"anchored at the start", []string{"https://id.a.example/realms/b"}, "xhttps://id.a.example/realms/b", false},
	{"prefix of the pattern", []string{"https://id.a.example/realms/bc"}, "https://id.a.example/realms/b", false},
	{"star at the end", []string{"https://*"}, "https://id.x.example", true},
	{"star at the end stops at a slash", []string{"https://*"}, "https://id.x.example/auth/realms/y", false},
	{"star does not cross a slash", []string{"https://id.a.example/*"}, "https://id.a.example/x/y/z", false},
	{"star is one segment", []string{"https://id.a.example/*"}, "https://id.a.example/x", true},
	{"star segment in the middle", []string{"https://login.example/*/v2.0"}, "https://login.example/tenant-id/v2.0", true},
	{"star segment in the middle is one segment", []string{"https://login.example/*/v2.0"}, "https://login.example/a/b/v2.0", false},
	{"star matches nothing", []string{"https://id.a.example/realms/b*"}, "https://id.a.example/realms/b", true},
	{"star in the host", []string{"https://*.a.example/realms/b"}, "https://id.a.example/realms/b", true},
	{"star in the host crosses dots", []string{"https://*.a.example/realms/b"}, "https://x.y.a.example/realms/b", true},
	{"star in the host is not a suffix match", []string{"https://*.a.example/realms/b"}, "https://id.a.example.evil/realms/b", false},
	{"star in the host does not match a path", []string{"https://*.a.example/realms/b"}, "https://other/x.a.example/realms/b", false},
	{"star in the host does not reach the path", []string{"https://*.a.example/realms/b"}, "https://other.example/.a.example/realms/b", false},
	{"two stars", []string{"https://*.a.example/*/b"}, "https://id.a.example/realms/b", true},
	{"double star", []string{"https://**"}, "https://id", true},
	{"double star is no wider", []string{"https://**"}, "https://id/r", false},
	{"star alone", []string{"*"}, "anything at all", true},
	{"star alone and a slash", []string{"*"}, "https://id", false},
	{"star alone and empty issuer", []string{"*"}, "", true},
	{"empty pattern and empty issuer", []string{""}, "", true},
	{"empty pattern", []string{""}, "https://id.a.example/realms/b", false},
	{"star does not match a newline", []string{"https://*"}, "https://id\n.a.example", false},
	{"case of the host", []string{"https://id.a.example/realms/b"}, "https://ID.A.EXAMPLE/realms/b", false},
	{"case of the scheme", []string{"https://id.a.example/realms/b"}, "HTTPS://id.a.example/realms/b", false},
	{"case of the path", []string{"https://id.a.example/realms/B"}, "https://id.a.example/realms/b", false},
	{"trailing slash on the issuer", []string{"https://id.a.example/realms/b"}, "https://id.a.example/realms/b/", false},
	{"trailing slash on the pattern", []string{"https://id.a.example/realms/b/"}, "https://id.a.example/realms/b", false},
	{"trailing slash under a star", []string{"https://id.a.example/realms/*"}, "https://id.a.example/realms/b/", false},
	{"realm under a star", []string{"https://id.a.example/auth/realms/*"}, "https://id.a.example/auth/realms/b", true},
	{"scheme http against https", []string{"https://id.a.example/realms/b"}, "http://id.a.example/realms/b", false},
	{"scheme under a star", []string{"*://id.a.example/realms/b"}, "http://id.a.example/realms/b", true},
	{"dot is literal", []string{"https://id.a.example/realms/b"}, "https://idxa.example/realms/b", false},
	{"question mark is literal", []string{"https://id.a.example/realms/b?"}, "https://id.a.example/realms/", false},
	{"plus is literal", []string{"https://id.a.example/realms/b+"}, "https://id.a.example/realms/bb", false},
	{"brackets are literal", []string{"https://id.a.example/realms/[bc]"}, "https://id.a.example/realms/b", false},
	{"brackets match themselves", []string{"https://id.a.example/realms/[bc]"}, "https://id.a.example/realms/[bc]", true},
	{"alternation is literal", []string{"https://a|https://b"}, "https://b", false},
	{"backslash is literal", []string{`https://id\.a`}, `https://id\.a`, true},
	{"dollar and caret are literal", []string{"^https://a$"}, "https://a", false},
	{"second pattern matches", []string{"https://x.example/r", "https://id.a.example/realms/*"}, "https://id.a.example/realms/b", true},
	{"no pattern matches", []string{"https://x.example/r", "https://y.example/*"}, "https://id.a.example/realms/b", false},
	{"port is literal", []string{"https://id.a.example/realms/b"}, "https://id.a.example:443/realms/b", false},
	{"unicode", []string{"https://ïd.example/*"}, "https://ïd.example/r", true},
}

func TestAllowed(t *testing.T) {
	for _, c := range issuerAllowedCases {
		t.Run(c.name, func(t *testing.T) {
			// Twice: the second call reads the compiled pattern.
			for range 2 {
				if got := Allowed(c.patterns, c.issuer); got != c.want {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}
