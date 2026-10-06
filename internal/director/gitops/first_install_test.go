/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A new tenant's manifest carries `apps: []`. The first install has to turn
// that into the list, not write a second `apps:` key beside it: a manifest
// with the key twice does not build, and then nothing in the tenant syncs.
func TestFirstInstallIntoANewTenantsManifest(t *testing.T) {
	text := tenantManifest("demo", "Demo", false)
	if !strings.Contains(text, "apps: []") {
		t.Fatalf("the template no longer writes an empty list; this test needs the form it does write:\n%s", text)
	}
	out, ok := insertAppProfile(text, "mathesar-ce", "sha256:"+strings.Repeat("a", 64), "", false)
	if !ok {
		t.Fatal("not inserted")
	}
	if n := strings.Count(out, "apps:"); n != 1 {
		t.Fatalf("apps: appears %d times:\n%s", n, out)
	}
	// Strict, so that a key written twice is an error and not the last one
	// winning.
	var doc map[string]any
	if err := yaml.UnmarshalStrict([]byte(out), &doc); err != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", err, out)
	}
	spec, _ := doc["spec"].(map[string]any)
	apps, _ := spec["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("apps = %v\n%s", apps, out)
	}
	app, _ := apps[0].(map[string]any)
	if app["profile"] != "mathesar-ce" || app["digest"] == nil {
		t.Fatalf("app = %v\n%s", app, out)
	}
}

// The same first install, made for everyone: the grant is a key of the entry
// the install writes, in the form a new tenant's manifest has, and it reads
// back as the boolean the Tenant's schema declares.
func TestFirstInstallForEveryoneIntoANewTenantsManifest(t *testing.T) {
	text := tenantManifest("demo", "Demo", false)
	if !strings.Contains(text, "apps: []") {
		t.Fatalf("the template no longer writes an empty list; this test needs the form it does write:\n%s", text)
	}
	out, ok := insertAppProfile(text, "mathesar-ce", "sha256:"+strings.Repeat("a", 64), "main", true)
	if !ok {
		t.Fatal("not inserted")
	}
	if n := strings.Count(out, "apps:"); n != 1 {
		t.Fatalf("apps: appears %d times:\n%s", n, out)
	}
	var doc struct {
		Spec struct {
			Apps []App `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", err, out)
	}
	if len(doc.Spec.Apps) != 1 {
		t.Fatalf("apps = %+v\n%s", doc.Spec.Apps, out)
	}
	if app := doc.Spec.Apps[0]; app.Profile != "mathesar-ce" || app.Digest == "" || !app.DefaultGrant {
		t.Fatalf("app = %+v\n%s", app, out)
	}
	var strict map[string]any
	if err := yaml.UnmarshalStrict([]byte(out), &strict); err != nil {
		t.Fatalf("the manifest does not parse strictly: %v\n%s", err, out)
	}
}
