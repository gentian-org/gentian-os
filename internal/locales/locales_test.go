/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package locales_test

import (
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/locales"
)

// Which languages a realm offers is declared state, so this only tidies what
// was declared and says what a realm gets before anybody has declared anything.
func TestNormaliseLocales(t *testing.T) {
	for name, tc := range map[string]struct {
		declared []string
		want     string
	}{
		"nothing declared": {nil, "en,de"},
		"empty":            {[]string{}, "en,de"},
		"all blank":        {[]string{"", "  "}, "en,de"},
		"one":              {[]string{"de"}, "de"},
		"several":          {[]string{"en", "de", "fr"}, "en,de,fr"},
		"spaced":           {[]string{" en ", " de "}, "en,de"},
		"blanks dropped":   {[]string{"en", "", "de"}, "en,de"},
		"a new language":   {[]string{"en", "de", "pl"}, "en,de,pl"},
		"order preserved":  {[]string{"de", "en"}, "de,en"},
	} {
		got := strings.Join(locales.Normalise(tc.declared), ",")
		if got != tc.want {
			t.Errorf("%s: NormaliseLocales(%v) = %q, want %q", name, tc.declared, got, tc.want)
		}
	}
}

// The fallback is the platform's own set, and it is the languages the desktop
// ships catalogues for. If these drift apart, a person signs in translated and
// lands on an English desktop.
func TestTheDefaultIsTheLanguagesTheDesktopShips(t *testing.T) {
	if got := strings.Join(locales.Default, ","); got != "en,de" {
		t.Fatalf("default locales = %q; gentian-ui/frontend/src/locales has en and de", got)
	}
	if locales.DefaultLanguage != "en" {
		t.Fatalf("default locale = %q, want the language the source strings are written in", locales.DefaultLanguage)
	}
}
