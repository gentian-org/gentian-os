/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
	"github.com/gentian-org/gentian-os/internal/schemacheck/schemachecktest"
)

const repoRoot = "../.."

// The embedded definitions are the repository's, byte for byte, and all of
// them. `make manifests` copies them and verify-gen holds the copy to the
// original; this says the same from inside `go test`, where a stale copy
// would otherwise make every other test of this package pass against
// yesterday's fields.
func TestTheEmbeddedDefinitionsAreTheRepositorys(t *testing.T) {
	for _, c := range []struct{ source, pattern, embedded string }{
		{filepath.Join(repoRoot, "charts", "gentian-os", "crds"), "gentianos.io_*.yaml", filepath.Join("definitions", "crds")},
		{filepath.Join(repoRoot, "crossplane", "xrds"), "*.yaml", filepath.Join("definitions", "xrds")},
	} {
		sources, err := filepath.Glob(filepath.Join(c.source, c.pattern))
		if err != nil || len(sources) == 0 {
			t.Fatalf("no definitions in %s (%v)", c.source, err)
		}
		copies, _ := filepath.Glob(filepath.Join(c.embedded, "*.yaml"))
		var want, got []string
		for _, s := range sources {
			want = append(want, filepath.Base(s))
		}
		for _, s := range copies {
			got = append(got, filepath.Base(s))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s holds %v\nthe repository's %s holds %v\nrun `make manifests`", c.embedded, got, c.source, want)
			continue
		}
		for _, s := range sources {
			original, err := os.ReadFile(s)
			if err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join(c.embedded, filepath.Base(s)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, copied) {
				t.Errorf("%s differs from the embedded copy: run `make manifests`", s)
			}
		}
	}
}

func TestEveryDefinitionIsReadFromTheEmbeddedFiles(t *testing.T) {
	set := schemacheck.Embedded()
	if !strings.HasPrefix(set.Digest, "sha256:") || len(set.Digest) != len("sha256:")+64 {
		t.Fatalf("digest %q", set.Digest)
	}
	// An XRD yields the CRDs Crossplane generates from it: the composite, and
	// the claim when it offers one. These are the kinds the audit that asked
	// for this check found fields being lost from.
	for kind, want := range map[string]schemacheck.Definition{
		"Tenant":     {CRD: "tenants.gentianos.io", Source: schemacheck.SourceChart},
		"Component":  {CRD: "components.gentianos.io", Source: schemacheck.SourceChart},
		"App":        {CRD: "apps.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xapps.gentianos.io"},
		"XApp":       {CRD: "xapps.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xapps.gentianos.io"},
		"XTenant":    {CRD: "xtenants.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xtenants.gentianos.io"},
		"Cluster":    {CRD: "clusters.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xclusters.gentianos.io"},
		"Repository": {CRD: "repositories.gentianos.io", Source: schemacheck.SourceXRD, XRD: "xrepositories.gentianos.io"},
	} {
		got, ok := set.Kind(kind)
		if !ok {
			t.Errorf("%s is not embedded", kind)
			continue
		}
		if got.CRD != want.CRD || got.Source != want.Source || got.XRD != want.XRD {
			t.Errorf("%s: got %s from %s (xrd %q), want %s from %s (xrd %q)", kind, got.CRD, got.Source, got.XRD, want.CRD, want.Source, want.XRD)
		}
		if len(got.Versions["v1alpha1"].Properties) == 0 {
			t.Errorf("%s: no schema for v1alpha1", kind)
		}
	}
	// The tenant XRD offers no claim, so no claim CRD is expected of the
	// cluster for it: tenants.gentianos.io is the chart's.
	if def, _ := set.Kind("Tenant"); def.Source != schemacheck.SourceChart {
		t.Errorf("Tenant must come from the chart, not from the XRD")
	}
}

// A cluster serving exactly what the binary was built with has nothing
// missing, for every definition -- the check must not find fault with a
// current cluster.
func TestACurrentClusterHasNothingMissing(t *testing.T) {
	for _, def := range schemacheck.Embedded().Definitions {
		served := schemachecktest.CRD(def)
		for _, v := range served.Spec.Versions {
			if missing := schemacheck.Missing(def.Versions[v.Name], v.Schema.OpenAPIV3Schema); len(missing) > 0 {
				t.Errorf("%s: a current definition reports %v missing", def.CRD, missing)
			}
		}
	}
}

// The fields whose loss prompted this check are found missing on a cluster
// that predates them. Nothing in the package names these fields; they are
// here only because they are the ones it was built to notice.
func TestTheFieldsThatWereLostAreNoticed(t *testing.T) {
	set := schemacheck.Embedded()
	for _, c := range []struct{ kind, field string }{
		{"Tenant", "spec.apps[].digest"},
		{"Tenant", "spec.apps[].catalogue"},
		{"Tenant", "spec.apps[].defaultGrant"},
		{"Tenant", "spec.apps[].addonPins"},
		{"Component", "spec.addonPins"},
		{"Component", "spec.profileRef.digest"},
		{"App", "spec.pullSecrets"},
		{"XApp", "spec.pullSecrets"},
	} {
		def, _ := set.Kind(c.kind)
		older := schemachecktest.Without(t, schemachecktest.Kind(t, set, c.kind), c.field)
		got := schemacheck.Missing(def.Versions["v1alpha1"], older.Spec.Versions[0].Schema.OpenAPIV3Schema)
		if !reflect.DeepEqual(got, []string{c.field}) {
			t.Errorf("%s without %s: reported %v", c.kind, c.field, got)
		}
	}
}

// Every kind the operator's API package registers has a definition embedded.
//
// The API package is where a new kind starts. One that gets a Go type and a
// reconciler but whose definition never reaches charts/gentian-os/crds or
// crossplane/xrds would be written by the binaries and checked by nothing.
func TestEveryKindOfTheAPIHasADefinition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	set := schemacheck.Embedded()
	var uncovered []string
	for kind, typ := range scheme.KnownTypes(gentianov1alpha1.GroupVersion) {
		// A kind is a type with object metadata; the list types and the
		// options types every group version carries are not.
		if _, object := typ.FieldByName("ObjectMeta"); !object {
			continue
		}
		if _, ok := set.Kind(kind); !ok {
			uncovered = append(uncovered, kind)
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Fatalf("no definition is embedded for %v: the binaries can write these kinds and nothing checks that the cluster "+
			"serves their fields. Their CRD belongs in charts/gentian-os/crds (or their XRD in crossplane/xrds); then run `make manifests`", uncovered)
	}
}
