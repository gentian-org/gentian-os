/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"strings"
	"testing"
	"testing/fstest"
)

// A repository is judged by what the release takes away: a field it no
// longer declares, in a whole object or a kustomize patch; a kind or a
// version it does not serve; a file that does not parse. A patch that
// leaves fields out, other groups' objects, hidden directories and files
// that are not YAML are not reported.
func TestARepositoryIsJudgedByWhatTheReleaseTakesAway(t *testing.T) {
	repo := fstest.MapFS{
		"clusters/c/tenants/team/groups.yaml": {Data: []byte(`apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: team
spec:
  groups:
    - name: sales
`)},
		"clusters/c/tenants/team/sign-in.yaml": {Data: []byte(`apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: team
spec:
  signInFrom:
    - tenant: other
      positions: [user]
`)},
		"clusters/c/two.yaml": {Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: x
data:
  anything: goes
---
apiVersion: gentianos.io/v1alpha1
kind: Gadget
metadata:
  name: g
---
apiVersion: gentianos.io/v9
kind: Tenant
metadata:
  name: future
`)},
		"clusters/c/broken.yaml":        {Data: []byte("apiVersion: gentianos.io/v1alpha1\nkind: [\n")},
		"clusters/c/README.md":          {Data: []byte("spec:\n  groups: []\n")},
		".github/workflows/ci.yaml":     {Data: []byte("apiVersion: gentianos.io/v1alpha1\nkind: Gadget\n")},
		"clusters/c/kustomization.yaml": {Data: []byte("resources: [groups.yaml]\n")},
	}
	found, err := Embedded().CheckRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range found {
		got = append(got, f.String())
	}
	want := []string{
		"clusters/c/broken.yaml: -: does not parse",
		"clusters/c/tenants/team/groups.yaml: Tenant/team: spec.groups: field not declared in schema",
		"clusters/c/two.yaml: Gadget/g: the kind Gadget is not served",
		`clusters/c/two.yaml: Tenant/future: Tenant is not served at version "v9"`,
	}
	if len(got) != len(want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
}
