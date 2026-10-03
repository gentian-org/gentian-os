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

package authz

import (
	"strings"
	"testing"
)

// The denied page wears the cluster's brand, loaded by the browser from where
// the issuer's host publishes it; without one it keeps the platform's colours.
func TestTheDeniedPageWearsTheBrand(t *testing.T) {
	if got := BrandingBase("https://id.k.example/auth"); got != "https://id.k.example/branding/" {
		t.Fatalf("branding base = %q", got)
	}
	if got := BrandingBase("http://keycloak.svc:8080/auth"); got != "" {
		t.Fatalf("a non-TLS issuer gave %q", got)
	}
	page := deniedPage(BrandingBase("https://id.k.example/auth/"))
	if !strings.Contains(page, `<link rel="stylesheet" href="https://id.k.example/branding/brand.css">`) {
		t.Fatalf("no brand stylesheet:\n%s", page)
	}
	if !strings.Contains(page, "var(--brand-color-brand-500,#262696)") {
		t.Fatal("the button lost its fallback colour")
	}
	if strings.Contains(deniedPage(""), "<link") {
		t.Fatal("a page without a brand still links one")
	}
}
