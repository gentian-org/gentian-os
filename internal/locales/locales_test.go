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
