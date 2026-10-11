/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package locales is the platform's own language set (AD-15).
//
// One package because two processes need the same answer and must not drift:
// the operator writes the kernel realm's languages, and the director tells a
// console what a tenant gets when it declares none. Two copies of the same
// list is a pair that agrees until somebody edits one.
//
// Which languages a realm actually offers is NOT here. That is an
// administrator's choice: it is declared on the Tenant, changed through the
// director, and applied from git. What is here is only what a realm gets
// before anybody has chosen.
package locales

import "strings"

// Default is the languages the platform ships its own strings in, and what a
// realm offers until a tenant says otherwise. ISO 639-1, which is what
// Keycloak's realm representation takes.
//
// It matches the catalogues in gentian-ui/frontend/src/locales. If these drift
// apart, a person signs in translated and lands on an English desktop.
var Default = []string{"en", "de"}

// DefaultLanguage answers a browser asking for a language that is not offered.
// English, because that is the language the platform's own strings are written
// in: a fallback should be the author's own words rather than a guess.
const DefaultLanguage = "en"

// Normalise drops blanks and falls back to Default.
//
// Languages, not locales: Keycloak serves de-CH from its German catalogue, so
// a realm listing regional codes offers a picker full of entries that render
// identically.
func Normalise(declared []string) []string {
	out := make([]string, 0, len(declared))
	for _, lang := range declared {
		if lang = strings.TrimSpace(lang); lang != "" {
			out = append(out, lang)
		}
	}
	if len(out) == 0 {
		return Default
	}
	return out
}
