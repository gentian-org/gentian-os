/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

func sources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(raw)
	}
	return out
}

// Every kind this package writes a manifest of has a definition embedded.
//
// The check reads a commit against the definitions the binary carries, and
// passes over a kind it has none for -- it cannot say what the cluster would
// do with one. So a manifest of a kind added here without its definition
// would be committed unchecked, and this is what says so: every kind spelled
// in this package's sources, as YAML or as a map key, is either embedded or
// listed below as belonging to somebody else's API.
func TestEveryKindTheDirectorWritesIsChecked(t *testing.T) {
	notOurs := map[string]bool{
		// kustomize's own file, which lists the manifests and is not one.
		"Kustomization": true,
	}
	spelled := regexp.MustCompile(`(?:\bkind: |"kind": *")([A-Z][A-Za-z]+)`)
	set := schemacheck.Embedded()
	found := map[string]bool{}
	for file, text := range sources(t) {
		for _, m := range spelled.FindAllStringSubmatch(text, -1) {
			kind := m[1]
			found[kind] = true
			if notOurs[kind] {
				continue
			}
			if _, ok := set.Kind(kind); !ok {
				t.Errorf("%s writes a manifest of kind %s, and no definition of it is embedded: its fields would be committed "+
					"without being put to what the cluster serves. Add its CRD or XRD and run `make manifests`, "+
					"or list it here if it is not of this API group", file, kind)
			}
		}
	}
	// The pattern has to keep finding the kinds this package is known to
	// write, or it has stopped matching how they are spelled.
	var missing []string
	for _, kind := range []string{"Tenant", "TenantDomain", "AppGrant", "BackupPolicy", "PlatformSecurityPolicy", "ComponentProfile", "Repository"} {
		if !found[kind] {
			missing = append(missing, kind)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("the scan no longer finds %v in this package's sources", missing)
	}
	// Two kinds are written without their name appearing: the Cluster claim
	// is edited in place, and Branding is marshalled from its Go type.
	for _, kind := range []string{"Cluster", "Branding"} {
		if _, ok := set.Kind(kind); !ok {
			t.Errorf("no definition of %s is embedded", kind)
		}
	}
}

// There is one place a commit is made and one place it is pushed, and the
// check stands in front of it. A second `git commit` anywhere in this
// package would be a way to write a manifest the check never sees.
func TestEveryCommitPassesTheDefinitionsCheck(t *testing.T) {
	commits, pushes := 0, 0
	for file, text := range sources(t) {
		commits += strings.Count(text, `"commit", "-m"`)
		pushes += strings.Count(text, `, "push")`)
		if file != "gitops.go" && (strings.Contains(text, `"commit", "-m"`) || strings.Contains(text, `, "push")`)) {
			t.Errorf("%s commits or pushes on its own; every write goes through commitPaths", file)
		}
	}
	if commits != 1 || pushes != 1 {
		t.Fatalf("found %d commits and %d pushes; want one of each, in commitPaths", commits, pushes)
	}
	body := sources(t)["gitops.go"]
	check := strings.Index(body, "g.checkDefinitions(ctx)")
	commit := strings.Index(body, `"commit", "-m"`)
	if check < 0 || commit < 0 || check > commit {
		t.Fatal("commitPaths must call checkDefinitions before it commits")
	}
}
