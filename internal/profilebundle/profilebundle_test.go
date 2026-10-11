/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// wiki is a bundle as a catalogue source publishes one. It leaves out what
// the schema defaults -- the database's engine and databasePerTenant, the
// tile's object -- which is what a published profile does.
const wiki = `# A comment, which is part of what was hashed and of nothing else.
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: wiki
  labels:
    gentianos.io/family: wiki
  annotations:
    gentianos.io/keycloak-group-attributes: '{"tier":["gold"]}'
spec:
  classes: [app]
  launch: tile
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/wiki
      name: wiki
      version: "1.0.0"
    extraValues:
      replicas: 2
      nested: {b: 1, a: [x, z]}
  requires:
    services:
      database: {}
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: wiki
      backend: {service: wiki, port: 8080}
      tile:
        displayName: Wiki
        path: /
        relation: can_launch
`

// held is the profile as the cluster holds it once the bundle was applied:
// the document read through the operator's type, with the defaults the API
// server fills in written here by hand, and the bundle in its annotation.
// By hand on purpose: taking them from the code under test would compare it
// with itself. The API server's own answer is asked in the controller tests.
func held(t *testing.T, bundle string) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	p := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.Unmarshal([]byte(bundle), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Requires != nil && p.Spec.Requires.Services != nil && p.Spec.Requires.Services.Database != nil {
		p.Spec.Requires.Services.Database.Engine = "postgresql"
		p.Spec.Requires.Services.Database.DatabasePerTenant = true
	}
	for i := range p.Spec.Expose {
		if p.Spec.Expose[i].Tile != nil {
			p.Spec.Expose[i].Tile.Object = "app"
		}
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[Annotation] = Encode([]byte(bundle))
	// What arrives besides: Argo CD's own bookkeeping.
	p.Annotations["argocd.argoproj.io/tracking-id"] = "gentian-catalogue-dev:gentianos.io/ComponentProfile:/" + p.Name
	return p
}

func refused(t *testing.T, what string, got *Refusal, reason string, says ...string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: verified", what)
	}
	if got.Reason != reason {
		t.Fatalf("%s: reason %s, want %s (%s)", what, got.Reason, reason, got.Message)
	}
	for _, s := range says {
		if !strings.Contains(got.Message, s) {
			t.Fatalf("%s: the refusal does not say %q: %s", what, s, got.Message)
		}
	}
}

func TestTheProfileTheBundleDescribesIsVerified(t *testing.T) {
	if r := Verify(held(t, wiki), Digest([]byte(wiki))); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
	// The digest is the catalogue's: sha256 over the published bytes.
	if d := Digest([]byte("abc")); d != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Digest = %s", d)
	}
}

// The bytes beside the profile are another build: whoever put them there,
// they do not hash to what the install is pinned to.
func TestABundleThatIsAnotherBuildIsRefused(t *testing.T) {
	other := strings.ReplaceAll(wiki, `version: "1.0.0"`, `version: "2.0.0"`)
	pinned := Digest([]byte(wiki))
	refused(t, "another build", Verify(held(t, other), pinned), ReasonMismatch,
		Short(pinned), Short(Digest([]byte(other))))
}

// The bytes are the pinned build and the profile is not what they say. This
// is the case two fields written by the same hand could never catch.
func TestAProfileThatIsNotWhatItsBundleSaysIsRefused(t *testing.T) {
	pinned := Digest([]byte(wiki))
	for what, c := range map[string]struct {
		edit func(*gentianov1alpha1.ComponentProfile)
		says string
	}{
		"another chart version": {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Package.Chart.Version = "6.6.6" }, "spec.package"},
		"another chart repository": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Package.Chart.Repository = "oci://evil.invalid/wiki"
		}, "spec.package"},
		"a value added to the chart's": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Package.ExtraValues.Raw = []byte(`{"replicas":2,"nested":{"a":["x","z"],"b":1},"image":"evil"}`)
		}, "spec.package"},
		"an exposure added": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Expose = append(p.Spec.Expose, p.Spec.Expose[0])
		}, "spec.expose"},
		"a field the bundle does not state": {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.DefaultForTenants = true }, "spec.defaultForTenants"},
		"a default switched off": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Requires.Services.Database.DatabasePerTenant = false
		}, "spec.requires"},
		"a requirement removed": {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Requires = nil }, "spec.requires"},
		"an annotation the platform reads, changed": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Annotations["gentianos.io/keycloak-group-attributes"] = `{"tier":["platinum"]}`
		}, "annotation gentianos.io/keycloak-group-attributes"},
		"an annotation the platform reads, added": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Annotations["gentianos.io/platform-app"] = "true"
		}, "annotation gentianos.io/platform-app"},
		"a label the platform reads, removed": {func(p *gentianov1alpha1.ComponentProfile) {
			delete(p.Labels, "gentianos.io/family")
		}, "label gentianos.io/family"},
	} {
		p := held(t, wiki)
		c.edit(p)
		refused(t, what, Verify(p, pinned), ReasonMismatch, Short(pinned), c.says)
	}
}

// How a free-form value's keys are ordered, and bookkeeping nothing here
// reads, are not differences.
func TestWhatSaysTheSameIsNotADifference(t *testing.T) {
	p := held(t, wiki)
	p.Spec.Package.ExtraValues.Raw = []byte(`{ "nested": {"a": ["x","z"], "b": 1}, "replicas": 2 }`)
	p.Labels["app.kubernetes.io/instance"] = "gentian-catalogue-dev"
	if r := Verify(p, Digest([]byte(wiki))); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
}

// A bundle is read the way the cluster received it. The catalogue directory
// is a kustomization, and kustomize hands a bare yes, on or y to the API
// server as a string, an old-style octal as a number and a date as a
// timestamp string; these are what `kustomize build` printed for this
// document, with the one number too large for an integer as the API server
// then stores it.
func TestABundleIsReadTheWayKustomizeHandsItOn(t *testing.T) {
	bundle := strings.Replace(wiki, "nested: {b: 1, a: [x, z]}",
		"nested: {b: 1, a: [x, y, no, On, ~, 0777, 1e3, 1.0, 0x10, 12345678901234567890, 2001-12-14]}\n      enabled: yes\n      exact: 9007199254740993", 1)
	p := held(t, wiki)
	p.Annotations[Annotation] = Encode([]byte(bundle))
	p.Spec.Package.ExtraValues.Raw = []byte(`{"replicas":2,"enabled":"yes","exact":9007199254740993,"nested":{"b":1,` +
		`"a":["x","y","no","On",null,511,1000,1,16,12345678901234567000,"2001-12-14T00:00:00Z"]}}`)
	if r := Verify(p, Digest([]byte(bundle))); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
	// And an integer that only differs past what a float holds is a difference.
	p.Spec.Package.ExtraValues.Raw = bytes.Replace(p.Spec.Package.ExtraValues.Raw,
		[]byte("9007199254740993"), []byte("9007199254740992"), 1)
	refused(t, "a large number changed in its last digit", Verify(p, Digest([]byte(bundle))), ReasonMismatch, "spec.package")
}

// Nothing to check against is not a pass. A profile that never had a bundle,
// or whose bundle was taken away, is held exactly like one that differs --
// otherwise removing the annotation would be the way round the check.
func TestAProfileWithNothingToCheckItAgainstIsRefused(t *testing.T) {
	pinned := Digest([]byte(wiki))

	none := held(t, wiki)
	delete(none.Annotations, Annotation)
	refused(t, "no bundle", Verify(none, pinned), ReasonUnverifiable, Short(pinned), Annotation)

	garbled := held(t, wiki)
	garbled.Annotations[Annotation] = "not base64 !"
	refused(t, "an unreadable bundle", Verify(garbled, pinned), ReasonUnverifiable)

	// Bytes that hash as pinned and are not a profile at all.
	notYAML := "\tnot: [a profile"
	odd := held(t, wiki)
	odd.Annotations[Annotation] = Encode([]byte(notYAML))
	refused(t, "a bundle that is no document", Verify(odd, Digest([]byte(notYAML))), ReasonUnverifiable)

	// Somebody else's bundle, correctly hashed, on this profile.
	other := strings.Replace(wiki, "name: wiki\n  labels", "name: blog\n  labels", 1)
	borrowed := held(t, wiki)
	borrowed.Annotations[Annotation] = Encode([]byte(other))
	refused(t, "another profile's bundle", Verify(borrowed, Digest([]byte(other))), ReasonMismatch, `"blog"`)
}

// The schema the operator defaults a bundle with is the one the chart
// installs. `make manifests` writes both; this is what notices one written
// without the other.
func TestTheEmbeddedSchemaIsTheOneTheChartInstalls(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "crd", "gentianos.io_componentprofiles.yaml"),
		filepath.Join("..", "..", "charts", "gentian-os", "crds", "gentianos.io_componentprofiles.yaml"),
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, crd) {
			t.Fatalf("componentprofiles.crd.yaml is not %s: run `make manifests`", path)
		}
	}
	if _, err := profileSchema(); err != nil {
		t.Fatal(err)
	}
}

// Defaults are applied where the API server applies them: to a property that
// is absent from an object that exists, inside lists and maps, and through a
// default that is itself an object. Never to an object that is not there.
func TestDefaultsAreAppliedOnlyInsideWhatExists(t *testing.T) {
	said, err := decodeText(wiki)
	if err != nil {
		t.Fatal(err)
	}
	db := said.Spec.Requires.Services.Database
	if db.Engine != "postgresql" || !db.DatabasePerTenant {
		t.Fatalf("database = %+v", db)
	}
	if said.Spec.Expose[0].Tile.Object != "app" {
		t.Fatalf("tile = %+v", said.Spec.Expose[0].Tile)
	}
	if said.Spec.Backup != nil || said.Spec.Requires.Services.Cache != nil || said.Spec.Package.API != nil {
		t.Fatalf("a default made an object that was not there: %+v", said.Spec)
	}
	// What the bundle states is kept, default or not.
	stated, err := decodeText(strings.Replace(wiki, "database: {}", "database: {engine: mariadb, databasePerTenant: false}", 1))
	if err != nil {
		t.Fatal(err)
	}
	db = stated.Spec.Requires.Services.Database
	if db.Engine != "mariadb" || db.DatabasePerTenant {
		t.Fatalf("database = %+v", db)
	}
}

// decodeText reads a one-document bundle's profile as Verify does.
func decodeText(bundle string) (*gentianov1alpha1.ComponentProfile, error) {
	docs, err := documents([]byte(bundle))
	if err != nil {
		return nil, err
	}
	return decode(docs[0])
}
