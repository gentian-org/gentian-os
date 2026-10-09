/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requires.services.identity.oidc.backchannelLogout: where an app is told
// that a person signed out. A profile says a path and which of its entries
// serves it, and the definition holds each part to what the address built
// from them needs: the realm posts a logout token to that address, so a
// profile must not be able to make it any address but its own Service's.
//
// The free-form backchannelLogoutUrl it replaces is refused by name, with a
// message that says what to write instead.
func TestBackchannelLogoutIsAPathOnTheProfilesOwnEntry(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")

	node := v.schema
	for _, name := range []string{"spec", "requires", "services", "identity", "oidc", "backchannelLogout"} {
		next, ok := node.Properties[name]
		if !ok {
			t.Fatalf("the definition has no %s on the way to spec.requires.services.identity.oidc.backchannelLogout", name)
		}
		node = &next
	}
	for _, field := range []string{"exposure", "path"} {
		if _, ok := node.Properties[field]; !ok {
			t.Fatalf("backchannelLogout has no %s", field)
		}
	}

	head := "  classes: [app]\n  launch: none\n  trustTier: certified\n"
	client := func(extra string) string {
		return "  requires:\n    services:\n      identity:\n        oidc:\n          clientId: demo\n" + extra
	}
	notice := func(exposure, path string) string {
		return client("          backchannelLogout:\n            exposure: " + exposure + "\n            path: \"" + path + "\"\n")
	}
	entry := func(name, backend string) string {
		return "  expose:\n    - name: " + name + "\n      surface: gateway\n      authMode: oidc\n      backend: " + backend + "\n"
	}
	own := entry("web", "{service: demo-web, port: 8080}")

	const notAnEntry = "backchannelLogout.exposure must name an entry of this profile"
	for _, c := range []struct{ name, spec, want string }{
		{"a path on the profile's own entry", head + notice("web", "/oidc/backchannel-logout") + own, ""},
		{"a path with the characters real apps use", head + notice("web", "/_synapse/client/oidc/backchannel_logout") + own, ""},
		{"a client that declares none", head + client("") + own, ""},

		{"a path and no entry", head + client("          backchannelLogout: {path: /logout}\n") + own, "exposure"},
		{"an entry and no path", head + client("          backchannelLogout: {exposure: web}\n") + own, "path"},

		{"the withdrawn free-form address", head + client("          backchannelLogoutUrl: \"https://demo.${TENANT_DOMAIN}/logout\"\n") + own,
			"backchannelLogoutUrl is withdrawn: declare backchannelLogout"},
		{"the withdrawn address beside the new field", head + client("          backchannelLogoutUrl: \"https://elsewhere.example/collect\"\n          backchannelLogout: {exposure: web, path: /logout}\n") + own,
			"backchannelLogoutUrl is withdrawn"},

		{"an entry the profile does not have", head + notice("api", "/logout") + own, notAnEntry},
		{"no entry at all", head + notice("web", "/logout"), notAnEntry},
		{"an entry that routes to another component", head + notice("web", "/logout") + entry("web", "{component: another-app, service: another-web, port: 8080}"), notAnEntry},
		{"a backend that is an address and not a Service's name", head + notice("web", "/logout") + entry("web", "{service: \"collect.elsewhere.example\", port: 8080}"), notAnEntry},
		{"a backend that would end the host and begin a path", head + notice("web", "/logout") + entry("web", "{service: \"elsewhere.example/x?\", port: 8080}"), notAnEntry},
		{"a backend that carries credentials for another host", head + notice("web", "/logout") + entry("web", "{service: \"a@elsewhere\", port: 8080}"), notAnEntry},

		{"an address where a path belongs", head + notice("web", "https://elsewhere.example/logout") + own, "path"},
		{"a path that starts a host", head + notice("web", "//elsewhere.example/logout") + own, "path"},
		{"a path with an empty segment", head + notice("web", "/a//logout") + own, "path"},
		{"a path that climbs", head + notice("web", "/a/../../logout") + own, "path"},
		{"a path with a segment that is a dot", head + notice("web", "/a/./logout") + own, "path"},
		{"a path that is only the root", head + notice("web", "/") + own, "path"},
		{"a path with a dot inside a segment", head + notice("web", "/index.php/apps/oidc/logout") + own, ""},
		{"a path with a query", head + notice("web", "/logout?to=elsewhere") + own, "path"},
		{"a path with a fragment", head + notice("web", "/logout#x") + own, "path"},
		{"a path with a user and a host in it", head + notice("web", "/@elsewhere.example/logout") + own, "path"},
		{"a path with a placeholder", head + notice("web", "/${TENANT_DOMAIN}/logout") + own, "path"},
		{"an empty path", head + notice("web", "") + own, "path"},
		{"a path longer than an address should be", head + notice("web", "/"+strings.Repeat("a", 256)) + own, "path"},

		{"an extension's client, which nothing serves", head + own + "  extensions:\n    - name: side\n      kernelRequirements:\n        identity:\n          oidc:\n            clientId: side\n            backchannelLogout: {exposure: web, path: /logout}\n",
			"not for an extension's"},
	} {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}
}

// The catalogue's own profiles, as gentian-apps publishes them
// (internal/profilebundle/testdata/bundles), are admitted by the definition:
// none still carries the withdrawn address, and the ones that say where they
// are told of a sign-out say it in a way the rules above accept. A catalogue
// and a definition that disagreed here would leave a cluster unable to take
// the catalogue's next version of an app.
func TestTheCataloguesProfilesDeclareSignOutAsTheDefinitionAdmits(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "profilebundle", "testdata", "bundles", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no bundles to read: %v", err)
	}
	told := map[string]bool{}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var profile string
		for _, doc := range strings.Split("\n"+string(raw), "\n---") {
			if strings.Contains(doc, "\nkind: ComponentProfile\n") {
				profile = doc
				break
			}
		}
		if profile == "" {
			t.Fatalf("%s: no ComponentProfile in the bundle", name)
		}
		if got := v.check(t, profile, ""); got != "" {
			t.Errorf("%s: the catalogue's profile is refused:\n%s", name, got)
		}
		if strings.Contains(profile, "\n          backchannelLogoutUrl:") {
			t.Errorf("%s: still carries the withdrawn backchannelLogoutUrl", name)
		}
		told[name] = strings.Contains(profile, "\n          backchannelLogout:\n")
	}
	// The two whose sign-out is shown end to end in gentian-apps
	// (e2e/oidc-sign-out) declare it.
	for _, name := range []string{"nextcloud-base-ce", "xwiki-ce"} {
		if !told[name] {
			t.Errorf("%s does not declare backchannelLogout", name)
		}
	}
}
