/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

func refused(t *testing.T, manifest string) string {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
		t.Fatal(err)
	}
	why, err := schemacheck.Embedded().Refused(obj)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(why, "\n")
}

// A profile of a class and a block this schema no longer has is refused,
// for the field Argo CD's diff stops at and for the value; a current one is
// taken as it is.
func TestAProfileOfARemovedShapeIsRefused(t *testing.T) {
	old := `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: odoo-adapter-ee}
spec:
  classes: [adapter]
  launch: none
  trustTier: certified
  version: "0.1.0"
  package: {chart: {repository: "oci://charts.example", name: odoo-adapter, version: "0.1.0"}}
  adapter:
    adapts: {profile: odoo-base-ce}
`
	got := refused(t, old)
	if !strings.Contains(got, "spec.adapter: field not declared in schema") || !strings.Contains(got, "Unsupported value") {
		t.Fatalf("an adapter's profile: %s", got)
	}
	current := strings.Replace(strings.Replace(old, "classes: [adapter]", "classes: [bridge]", 1), `  adapter:
    adapts: {profile: odoo-base-ce}
`, `  bridge:
    app: {profile: odoo-base-ce}
    ways:
    - {name: tools, caller: agents, callee: app, protocol: mcp, speaksAs: person}
`, 1)
	if got := refused(t, current); got != "" {
		t.Fatalf("a current profile is refused: %s", got)
	}
}

// A rule that reads a field the profile leaves to its default is judged as
// the API server judges it: with the default filled in. odoo-base-ce names
// the container of its quiesce and leaves mode at scaleDown.
func TestARuleReadingADefaultedFieldIsJudgedWithTheDefault(t *testing.T) {
	profile := `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: odoo-base-ce}
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "0.1.0"
  package: {chart: {repository: "oci://charts.example", name: odoo, version: "0.1.0"}}
  backup:
    quiesce:
      container: odoo
`
	if got := refused(t, profile); got != "" {
		t.Fatalf("a profile leaving quiesce.mode to its default is refused: %s", got)
	}
}

// Removed reports what a release took away, and nothing a rule finds: a
// profile breaking a rule is not of a removed shape.
func TestRemovedIsWhatAReleaseTookAway(t *testing.T) {
	read := func(manifest string) map[string]any {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	breaksARule := read(`apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: odoo-base-ce}
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "0.1.0"
  package: {chart: {repository: "oci://charts.example", name: odoo, version: "0.1.0"}}
  backup:
    quiesce: {mode: command}
`)
	if why, _ := schemacheck.Embedded().Refused(breaksARule); len(why) == 0 {
		t.Fatal("a profile breaking the quiesce rule is not refused")
	}
	if why, err := schemacheck.Embedded().Removed(breaksARule); err != nil || len(why) != 0 {
		t.Fatalf("a rule's refusal counts as removed: %v %v", why, err)
	}
	adapter := read(`apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata: {name: odoo-adapter-ee}
spec:
  classes: [adapter]
  launch: none
  trustTier: certified
  version: "0.1.0"
  package: {chart: {repository: "oci://charts.example", name: odoo-adapter, version: "0.1.0"}}
  adapter: {adapts: {profile: odoo-base-ce}}
`)
	why, err := schemacheck.Embedded().Removed(adapter)
	if err != nil || len(why) != 2 {
		t.Fatalf("an adapter's profile: %v %v", why, err)
	}
	// Invalid leaves out the field the API server drops, and keeps the value
	// it refuses.
	invalid, _ := schemacheck.Embedded().Invalid(adapter)
	if len(invalid) != 1 || !strings.Contains(invalid[0], "Unsupported value") {
		t.Fatalf("Invalid of an adapter's profile: %v", invalid)
	}
}
