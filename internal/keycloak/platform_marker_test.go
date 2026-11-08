/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every client and group a Composition makes in a realm carries the
// platform's mark, which is how a realm import knows not to make it from a
// bundle. A client the Composition only observes is made by a Job, which
// marks it in its own body.
func TestEveryClientAndGroupACompositionMakesIsMarked(t *testing.T) {
	mark := "                  " + PlatformMarkerAttribute + ": " + PlatformMarkerValue + "\n"
	files, err := filepath.Glob(filepath.Join("..", "..", "crossplane", "compositions", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Compositions: %v", err)
	}
	made := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range strings.Split(string(raw), "\n            ---\n") {
			kind := ""
			switch {
			case strings.Contains(doc, "apiVersion: openidclient.keycloak.crossplane.io/v1alpha1\n            kind: Client\n"),
				strings.Contains(doc, "apiVersion: samlclient.keycloak.crossplane.io/v1alpha1\n            kind: Client\n"):
				kind = "client"
			case strings.Contains(doc, "apiVersion: group.keycloak.crossplane.io/v1alpha1\n            kind: Group\n"):
				kind = "group"
			default:
				continue
			}
			if strings.Contains(doc, `managementPolicies: ["Observe"]`) {
				continue
			}
			made++
			name := regexp.MustCompile(`(?m)^              name: (.*)$`).FindStringSubmatch(doc)
			if kind == "client" && !strings.Contains(doc, mark) {
				t.Errorf("%s: the client %v is not marked as the platform's in its extraConfig", filepath.Base(file), name)
			}
			// app-default's entitlement group is given the mark among the
			// attributes its template sets; every other group carries it as is.
			if kind == "group" && !strings.Contains(doc, mark) && (!strings.Contains(doc, "toYaml $gAttrs") ||
				!strings.Contains(string(raw), `set $gAttrs "`+PlatformMarkerAttribute+`" "`+PlatformMarkerValue+`"`)) {
				t.Errorf("%s: the group %v is not marked as the platform's", filepath.Base(file), name)
			}
		}
	}
	if made < 10 {
		t.Errorf("found %d clients and groups the Compositions make; the split no longer finds them", made)
	}
}

// The names the platform gives its clients are the platform's by the
// patterns, for a bundle taken before the mark; a name the tenant might give
// a client, or Keycloak's own, is not.
func TestThePlatformsClientNamesMatchItsPatterns(t *testing.T) {
	platform := func(id string) bool {
		for _, p := range PlatformClientPatterns {
			if regexp.MustCompile(p).MatchString(id) {
				return true
			}
		}
		return false
	}
	for _, id := range []string{"gentian-edge-public", "gentian-edge-exchange", "vouch-google", "broker-acme",
		"broker-to-partner", "gentian-dovecot", "openbao", "https://wiki.acme.example/sso",
		"gentian-registrar-admin", "gentian-director-admin", "gentian-argocd", "headlamp", "gentian-cli", "litellm-dashboard"} {
		if !platform(id) {
			t.Errorf("%s is not known as the platform's", id)
		}
	}
	for _, id := range []string{"acme-wiki", "broker", "account", "realm-management", "https://wiki.acme.example/sso/x",
		"my-openbao", "gentian-dovecot-2"} {
		if platform(id) {
			t.Errorf("%s is taken for the platform's", id)
		}
	}
	group := regexp.MustCompile(PlatformGroupPattern)
	for name, want := range map[string]bool{
		TenantMembersGroup("acme"): true, TenantAppGroup("acme", "wiki"): true,
		"gentian:platform:admin": true, "staff": false, "gentian": false,
	} {
		if group.MatchString(name) != want {
			t.Errorf("%s: platform's = %v", name, !want)
		}
	}
}
