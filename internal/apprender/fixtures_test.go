/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The app Composition's render fixtures: an XR, the resources the
// Composition read, and what it rendered from them. Each is rendered here
// from the same inputs and has to come out the same, object for object.
// These fixtures were the Composition's tests; they are kept as this
// renderer's after the Composition is gone, which is how an app it installed
// is known to be taken over unchanged.
const fixtureRoot = "testdata/render"

func TestRendersWhatTheAppCompositionRendered(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(fixtureRoot, "app-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no fixtures under %s", fixtureRoot)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			in := fixtureInput(t, dir)
			got, err := Render(in)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			wantObjs, wantObserve := expectedObjects(t, filepath.Join(dir, "expected.yaml"))
			compareObjects(t, "objects", wantObjs, got.Objects)
			compareObjects(t, "observe-only objects", wantObserve, got.ObserveOnly)
		})
	}
}

// fixtureInput reads a fixture the way the Composition read its inputs.
func fixtureInput(t *testing.T, dir string) *Input {
	t.Helper()
	xr := readDocs(t, filepath.Join(dir, "xr.yaml"))[0]
	spec, _, _ := unstructured.NestedMap(xr.Object, "spec")
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(spec, path...)
		return s
	}
	app := str("claimRef", "name")
	in := &Input{
		App:           app,
		Namespace:     str("tenantNamespace"),
		Realm:         str("realm"),
		Domain:        str("domain"),
		ReleaseObject: xr.GetName() + "-release",
		HelmRelease:   app + "-release",
		KeepVolumes:   app + "-release-keep-volumes",
		ProfileName:   str("profileRef", "name"),
		Composite:     xr.GetName(),
		Consumer:      gentianov1alpha1.CredentialConsumer{Kind: "XApp", Name: xr.GetName()},
		Observed:      map[string]*unstructured.Unstructured{},
	}
	in.MailboxTokenSignIn, _, _ = unstructured.NestedBool(spec, "mailboxTokenSignIn")
	in.SignInSidecar, _, _ = unstructured.NestedMap(spec, "signInSidecar")
	in.Addons, _, _ = unstructured.NestedStringSlice(spec, "addons")
	if r, ok, _ := unstructured.NestedInt64(spec, "config", "replicas"); ok {
		in.Replicas = &r
	}
	in.ExtraValues, _, _ = unstructured.NestedMap(spec, "config", "extraValues")
	in.PullSecrets.Images, _, _ = unstructured.NestedStringSlice(spec, "pullSecrets", "images")
	charts, _, _ := unstructured.NestedSlice(spec, "pullSecrets", "charts")
	for _, c := range charts {
		m, _ := c.(map[string]interface{})
		repo, _ := m["repository"].(string)
		name, _ := m["secretName"].(string)
		in.PullSecrets.Charts = append(in.PullSecrets.Charts, ChartPullSecret{Repository: repo, SecretName: name})
	}

	var build *unstructured.Unstructured
	for _, obj := range readDir(t, filepath.Join(dir, "required-resources")) {
		switch obj.GetKind() {
		case "ComponentProfile":
			profile := &gentianov1alpha1.ComponentProfile{}
			convert(t, obj.Object, profile)
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
			convert(t, obj.Object, catalog)
			in.Packs = &catalog.Spec
		case "ClientPolicyProfilePolicy":
			in.RedirectPolicy = obj
		}
	}
	if build != nil && str("profileRef", "build") != "" {
		buildSpec, ok, _ := unstructured.NestedMap(build.Object, "status", "profileSpec")
		if !ok {
			t.Fatalf("build %s carries no profileSpec", build.GetName())
		}
		profile := &gentianov1alpha1.ComponentProfileSpec{}
		convert(t, buildSpec, profile)
		in.Profile = profile
	}
	for _, obj := range readDir(t, filepath.Join(dir, "observed-resources")) {
		in.Observed[obj.GetName()] = obj
	}
	return in
}

// expectedObjects reads what the Composition rendered, as the objects
// themselves: the composite and Crossplane's bookkeeping dropped, and an
// Object wrapper replaced by the manifest it wraps. A wrapper that only
// observes is answered separately.
func expectedObjects(t *testing.T, path string) (objs, observe []*unstructured.Unstructured) {
	t.Helper()
	for _, obj := range readDocs(t, path) {
		if obj.GetKind() == "XApp" {
			continue
		}
		// The Composition seeded each generated secret's path in the
		// tenant's store itself. The tenant's reconciler seeds the same
		// paths, write-once, and nothing here renders them.
		if obj.GetKind() == "SecretV2" || strings.HasSuffix(obj.GetName(), "-seed") {
			continue
		}
		if obj.GetKind() == "Object" && strings.HasPrefix(obj.GetAPIVersion(), "kubernetes.crossplane.io/") {
			manifest, _, _ := unstructured.NestedMap(obj.Object, "spec", "forProvider", "manifest")
			inner := &unstructured.Unstructured{Object: manifest}
			policies, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "managementPolicies")
			if len(policies) == 1 && policies[0] == "Observe" {
				observe = append(observe, inner)
			} else {
				objs = append(objs, inner)
			}
			continue
		}
		annotations := obj.GetAnnotations()
		delete(annotations, "crossplane.io/composition-resource-name")
		if len(annotations) == 0 {
			annotations = nil
		}
		obj.SetAnnotations(annotations)
		labels := obj.GetLabels()
		delete(labels, "crossplane.io/composite")
		if len(labels) == 0 {
			labels = nil
		}
		obj.SetLabels(labels)
		obj.SetOwnerReferences(nil)
		objs = append(objs, obj)
	}
	return objs, observe
}

// objectKey names an object for comparison. A Release by the Helm release
// it installs: its own name is the Release object's, which the operator
// chooses differently.
func objectKey(obj *unstructured.Unstructured) string {
	name := obj.GetName()
	if obj.GetKind() == "Release" {
		if ext := obj.GetAnnotations()["crossplane.io/external-name"]; ext != "" {
			name = "helm:" + ext
		}
	}
	return obj.GetAPIVersion() + "/" + obj.GetKind() + "/" + obj.GetNamespace() + "/" + name
}

func compareObjects(t *testing.T, what string, want, got []*unstructured.Unstructured) {
	t.Helper()
	index := func(objs []*unstructured.Unstructured) map[string]map[string]interface{} {
		m := map[string]map[string]interface{}{}
		for _, obj := range objs {
			norm := obj.DeepCopy()
			if norm.GetKind() == "Release" {
				// The Release object's own name is the operator's to choose.
				unstructured.RemoveNestedField(norm.Object, "metadata", "name")
			}
			if _, dup := m[objectKey(obj)]; dup {
				t.Errorf("%s: %s rendered twice", what, objectKey(obj))
			}
			m[objectKey(obj)] = normalize(t, norm.Object)
		}
		return m
	}
	w, g := index(want), index(got)
	// APPRENDER_ONLY narrows the comparison to the objects whose key it
	// matches, for working on one part at a time.
	if only := os.Getenv("APPRENDER_ONLY"); only != "" {
		re := regexp.MustCompile(only)
		for _, m := range []map[string]map[string]interface{}{w, g} {
			for k := range m {
				if !re.MatchString(k) {
					delete(m, k)
				}
			}
		}
	}
	keys := map[string]bool{}
	for k := range w {
		keys[k] = true
	}
	for k := range g {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		wo, inW := w[k]
		gotObj, inG := g[k]
		switch {
		case !inG:
			t.Errorf("%s: %s not rendered", what, k)
		case !inW:
			t.Errorf("%s: %s rendered, and the Composition did not", what, k)
		default:
			if diff := cmp.Diff(wo, gotObj); diff != "" {
				t.Errorf("%s: %s differs (-composition +rendered):\n%s", what, k, diff)
			}
		}
	}
}

// normalize passes an object through YAML, so that what the renderer built
// in Go compares with what was read from a file: numbers, empty maps and
// slices alike.
func normalize(t *testing.T, obj map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]interface{}{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func convert(t *testing.T, in map[string]interface{}, out interface{}) {
	t.Helper()
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(raw, out); err != nil {
		t.Fatal(err)
	}
}

func readDir(t *testing.T, dir string) []*unstructured.Unstructured {
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		objs = append(objs, readDocs(t, filepath.Join(dir, e.Name()))...)
	}
	return objs
}

func readDocs(t *testing.T, path string) []*unstructured.Unstructured {
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
		if len(m) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: m})
	}
	return objs
}
