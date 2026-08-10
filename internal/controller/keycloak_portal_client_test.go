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

package controller

import (
	"strings"
	"testing"
)

// Sign-out returns a tenant member to https://<tenant>.<kernel-domain>, because
// the apex would ask them for an email again. Keycloak validates
// post_logout_redirect_uri against the registered list, so if that host is not
// registered the logout fails outright with "Invalid redirect uri" rather than
// landing somewhere else.

func TestPortalClientRegistersTheTenantOriginForLogout(t *testing.T) {
	t.Parallel()
	script := buildPortalPublicClientScript(`"demo"`)

	if !strings.Contains(script, `TENANT_ORIGIN="https://${REALM}.${KERNEL_DOMAIN}"`) {
		t.Fatal("expected the tenant origin to be derived from the realm")
	}
	if !strings.Contains(script, `KERNEL_DOMAIN="${PORTAL#https://portal.}"`) {
		t.Fatal("expected the kernel domain to be derived from the portal origin")
	}
	if !strings.Contains(script, `($tenant + "/")`) {
		t.Fatal("expected the tenant origin among the registered URIs")
	}
	// Without this the browser cannot complete the logout redirect.
	post := script[strings.Index(script, "post.logout.redirect.uris"):]
	post = post[:strings.Index(post, "\n")+200]
	if !strings.Contains(post, "$tenant") {
		t.Fatal("tenant origin must be a registered post-logout redirect URI")
	}
}

func TestPortalClientStillAllowsThePortalOrigin(t *testing.T) {
	t.Parallel()
	script := buildPortalPublicClientScript(`"demo"`)
	// Operators sign in on the apex portal and must keep working.
	if !strings.Contains(script, `($portal + "/login")`) {
		t.Fatal("the portal origin must remain registered")
	}
}
