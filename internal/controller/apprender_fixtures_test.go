/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
)

// The app Composition's render fixtures live on as apprender's
// (internal/apprender/testdata/render). Where the operator and what a chart
// is rendered with have to agree -- a name, an address, an account -- the
// agreement is checked here by rendering a fixture, as the reconciler does.

// apprenderFixtureRoot is where the render fixtures are.
var apprenderFixtureRoot = filepath.Join("..", "apprender", "testdata", "render")

// apprenderFixtureNames are every app render fixture.
func apprenderFixtureNames(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(apprenderFixtureRoot, "app-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no render fixtures under %s", apprenderFixtureRoot)
	}
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		names = append(names, filepath.Base(d))
	}
	return names
}

// apprenderFixtureInput reads a fixture into what one render reads, the way
// apprender's own parity test does.
func apprenderFixtureInput(t *testing.T, fixture string) *apprender.Input {
	t.Helper()
	dir := filepath.Join(apprenderFixtureRoot, fixture)
	xr := yamlDocs(t, filepath.Join(dir, "xr.yaml"))[0]
	spec, _, _ := unstructured.NestedMap(xr.Object, "spec")
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(spec, path...)
		return s
	}
	app := str("claimRef", "name")
	in := &apprender.Input{
		App:           app,
		Namespace:     str("tenantNamespace"),
		Realm:         str("realm"),
		Domain:        str("domain"),
		ReleaseObject: xr.GetName() + composedReleaseSuffix,
		HelmRelease:   app + composedReleaseSuffix,
		KeepVolumes:   app + composedReleaseSuffix + "-keep-volumes",
		ProfileName:   str("profileRef", "name"),
		Composite:     xr.GetName(),
		Consumer:      gentianov1alpha1.CredentialConsumer{Kind: "XApp", Name: xr.GetName()},
		Observed:      map[string]*unstructured.Unstructured{},
	}
	in.MailboxTokenSignIn, _, _ = unstructured.NestedBool(spec, "mailboxTokenSignIn")
	in.SignInSidecar, _, _ = unstructured.NestedMap(spec, "signInSidecar")
	in.Addons, _, _ = unstructured.NestedStringSlice(spec, "addons")
	in.ExtraValues, _, _ = unstructured.NestedMap(spec, "config", "extraValues")
	in.PullSecrets.Images, _, _ = unstructured.NestedStringSlice(spec, "pullSecrets", "images")
	charts, _, _ := unstructured.NestedSlice(spec, "pullSecrets", "charts")
	for _, c := range charts {
		m, _ := c.(map[string]interface{})
		repo, _ := m["repository"].(string)
		name, _ := m["secretName"].(string)
		in.PullSecrets.Charts = append(in.PullSecrets.Charts, apprender.ChartPullSecret{Repository: repo, SecretName: name})
	}
	var build *unstructured.Unstructured
	for _, obj := range yamlDir(t, filepath.Join(dir, "required-resources")) {
		switch obj.GetKind() {
		case "ComponentProfile":
			profile := &gentianov1alpha1.ComponentProfile{}
			convertYAML(t, obj.Object, profile)
			in.Profile = &profile.Spec
			in.ProfileAnnotations = profile.Annotations
		case "ProfileBuild":
			build = obj
		case "ConfigMap":
			data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
			switch obj.GetLabels()["gentianos.io/config-type"] {
			case "cluster-config":
				in.ClusterConfig = data
				in.ClusterConfigNamespace = obj.GetNamespace()
			case "kernel-services":
				in.KernelServices = data
			}
		case "OIDCPackCatalog":
			catalog := &gentianov1alpha1.OIDCPackCatalog{}
			convertYAML(t, obj.Object, catalog)
			in.Packs = &catalog.Spec
		case "ClientPolicyProfilePolicy":
			in.RedirectPolicy = obj
		}
	}
	if build != nil && str("profileRef", "build") != "" {
		buildSpec, _, _ := unstructured.NestedMap(build.Object, "status", "profileSpec")
		profile := &gentianov1alpha1.ComponentProfileSpec{}
		convertYAML(t, buildSpec, profile)
		in.Profile = profile
	}
	for _, obj := range yamlDir(t, filepath.Join(dir, "observed-resources")) {
		in.Observed[obj.GetName()] = obj
	}
	return in
}

// renderFixture renders a fixture as it is.
func renderFixture(t *testing.T, fixture string) []*unstructured.Unstructured {
	t.Helper()
	return renderApp(t, apprenderFixtureInput(t, fixture))
}

// renderApp renders one app and answers its objects.
func renderApp(t *testing.T, in *apprender.Input) []*unstructured.Unstructured {
	t.Helper()
	if in.Observed == nil {
		in.Observed = map[string]*unstructured.Unstructured{}
	}
	out, err := apprender.Render(in)
	if err != nil {
		t.Fatalf("render %s: %v", in.App, err)
	}
	return out.Objects
}

// renderedOfKind are the rendered objects of one kind.
func renderedOfKind(objs []*unstructured.Unstructured, kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == kind {
			out = append(out, o)
		}
	}
	return out
}

// renderedNamed is the rendered object of a kind and name, nil when there is
// none.
func renderedNamed(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range renderedOfKind(objs, kind) {
		if o.GetName() == name {
			return o
		}
	}
	return nil
}

// chartRenderInput is the least a chart profile is rendered from, as the
// reconciler names a component it installs itself (renderNames) in tenant
// acme.
func chartRenderInput(app string, profile *gentianov1alpha1.ComponentProfile) *apprender.Input {
	release := "tenant-acme-" + app
	return &apprender.Input{
		App: app, Namespace: "tenant-acme", Domain: "acme.k.example",
		ReleaseObject: release, HelmRelease: release, KeepVolumes: app + "-keep-volumes", Composite: release,
		ProfileName: profile.Name, Profile: &profile.Spec, ProfileAnnotations: profile.Annotations,
		Placed:   operatorPlaced(profile),
		Consumer: gentianov1alpha1.CredentialConsumer{Kind: "Component", Name: app},
		Observed: map[string]*unstructured.Unstructured{},
	}
}

func yamlDocs(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var objs []*unstructured.Unstructured
	for {
		m := map[string]interface{}{}
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(m) > 0 {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}
	return objs
}

func yamlDir(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var objs []*unstructured.Unstructured
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			objs = append(objs, yamlDocs(t, filepath.Join(dir, e.Name()))...)
		}
	}
	return objs
}

func convertYAML(t *testing.T, in map[string]interface{}, out interface{}) {
	t.Helper()
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}
