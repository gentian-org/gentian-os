/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// issuerAllowedCases is the transcript of IssuerAllowed: anchoring, "*",
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
	{"star at the end", []string{"https://*"}, "https://id.x.example/auth/realms/y", true},
	{"star crosses slashes", []string{"https://id.a.example/*"}, "https://id.a.example/x/y/z", true},
	{"star matches nothing", []string{"https://id.a.example/realms/b*"}, "https://id.a.example/realms/b", true},
	{"star in the host", []string{"https://*.a.example/realms/b"}, "https://id.a.example/realms/b", true},
	{"star in the host crosses dots", []string{"https://*.a.example/realms/b"}, "https://x.y.a.example/realms/b", true},
	{"star in the host is not a suffix match", []string{"https://*.a.example/realms/b"}, "https://id.a.example.evil/realms/b", false},
	{"star in the host matches a path", []string{"https://*.a.example/realms/b"}, "https://evil/x.a.example/realms/b", true},
	{"two stars", []string{"https://*.a.example/*/b"}, "https://id.a.example/realms/b", true},
	{"double star", []string{"https://**"}, "https://id", true},
	{"star alone", []string{"*"}, "anything at all", true},
	{"star alone and empty issuer", []string{"*"}, "", true},
	{"empty pattern and empty issuer", []string{""}, "", true},
	{"empty pattern", []string{""}, "https://id.a.example/realms/b", false},
	{"star does not match a newline", []string{"https://*"}, "https://id\n.a.example", false},
	{"case of the host", []string{"https://id.a.example/realms/b"}, "https://ID.A.EXAMPLE/realms/b", false},
	{"case of the scheme", []string{"https://id.a.example/realms/b"}, "HTTPS://id.a.example/realms/b", false},
	{"case of the path", []string{"https://id.a.example/realms/B"}, "https://id.a.example/realms/b", false},
	{"trailing slash on the issuer", []string{"https://id.a.example/realms/b"}, "https://id.a.example/realms/b/", false},
	{"trailing slash on the pattern", []string{"https://id.a.example/realms/b/"}, "https://id.a.example/realms/b", false},
	{"trailing slash under a star", []string{"https://id.a.example/realms/*"}, "https://id.a.example/realms/b/", true},
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
	{"second pattern matches", []string{"https://x.example/r", "https://id.a.example/*"}, "https://id.a.example/realms/b", true},
	{"no pattern matches", []string{"https://x.example/r", "https://y.example/*"}, "https://id.a.example/realms/b", false},
	{"port is literal", []string{"https://id.a.example/realms/b"}, "https://id.a.example:443/realms/b", false},
	{"unicode", []string{"https://ïd.example/*"}, "https://ïd.example/r", true},
}

// TestIssuerAllowedCopiesAgree runs the operator's and the director's
// IssuerAllowed over the same inputs.
func TestIssuerAllowedCopiesAgree(t *testing.T) {
	for _, c := range issuerAllowedCases {
		t.Run(c.name, func(t *testing.T) {
			op := IssuerAllowed(c.patterns, c.issuer)
			dir := gitops.IssuerAllowed(c.patterns, c.issuer)
			if op != dir {
				t.Fatalf("operator %v, director %v", op, dir)
			}
			if op != c.want {
				t.Fatalf("got %v, want %v", op, c.want)
			}
		})
	}
}
