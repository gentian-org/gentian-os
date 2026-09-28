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

package authz_test

import (
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/authz"
)

// Adding a language must not mean editing Go (AD-15), so the list is read
// from the deployment and the constant is only what it falls back to.
func TestSupportedLocalesComeFromTheDeployment(t *testing.T) {
	for name, tc := range map[string]struct {
		configured string
		want       string
	}{
		"unset":           {"", "en,de"},
		"blank":           {"   ", "en,de"},
		"one":             {"de", "de"},
		"several":         {"en,de,fr", "en,de,fr"},
		"spaced":          {" en , de , fr ", "en,de,fr"},
		"empty entries":   {"en,,de,", "en,de"},
		"a new language":  {"en,de,pl", "en,de,pl"},
		"order preserved": {"de,en", "de,en"},
	} {
		got := strings.Join(authz.SupportedLocales(tc.configured), ",")
		if got != tc.want {
			t.Errorf("%s: SupportedLocales(%q) = %q, want %q", name, tc.configured, got, tc.want)
		}
	}
}

// The fallback is the platform's own set, and it is the languages the desktop
// ships catalogues for. If these drift apart, a person signs in translated and
// lands on an English desktop.
func TestTheDefaultIsTheLanguagesTheDesktopShips(t *testing.T) {
	if got := strings.Join(authz.DefaultSupportedLocales, ","); got != "en,de" {
		t.Fatalf("default locales = %q; gentian-ui/frontend/src/locales has en and de", got)
	}
	if authz.DefaultLocale != "en" {
		t.Fatalf("default locale = %q, want the language the source strings are written in", authz.DefaultLocale)
	}
}
