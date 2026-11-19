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
