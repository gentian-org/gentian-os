/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
