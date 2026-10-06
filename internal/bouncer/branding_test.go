/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"strings"
	"testing"
)

// The denied page wears the cluster's brand, loaded by the browser from the
// cluster's bare domain, where the concierge serves it; without one it keeps
// the platform's colours.
func TestTheDeniedPageWearsTheBrand(t *testing.T) {
	if got := BrandingBase("https://id.k.example/auth"); got != "https://k.example/branding/" {
		t.Fatalf("branding base = %q", got)
	}
	for _, issuer := range []string{"http://keycloak.svc:8080/auth", "https://login.k.example/auth", "https://id./auth"} {
		if got := BrandingBase(issuer); got != "" {
			t.Fatalf("issuer %q gave %q", issuer, got)
		}
	}
	page := deniedPage(BrandingBase("https://id.k.example/auth/"))
	if !strings.Contains(page, `<link rel="stylesheet" href="https://k.example/branding/brand.css">`) {
		t.Fatalf("no brand stylesheet:\n%s", page)
	}
	if !strings.Contains(page, "var(--brand-color-brand-500,#262696)") {
		t.Fatal("the button lost its fallback colour")
	}
	if strings.Contains(deniedPage(""), "<link") {
		t.Fatal("a page without a brand still links one")
	}
}
