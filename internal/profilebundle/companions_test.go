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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// catalogueNamespace is where the test's cluster applies its catalogue.
const catalogueNamespace = "kernel-provisioning"

// applied is a bundle as a cluster holds it once Argo CD has synced the
// catalogue directory: every document an object, read here by another YAML
// reader than the one under test, the namespaced ones in the catalogue's
// namespace, each with the label Argo CD tracks it by, and the profile
// carrying the bundle and where it came from.
func applied(t *testing.T, bundle, origin string) (*gentianov1alpha1.ComponentProfile, []client.Object) {
	t.Helper()
	return appliedFrom(t, bundle, bundle, origin)
}

// appliedFrom is applied for a rendering of the bundle that is not its own
// bytes: what kustomize printed for it.
func appliedFrom(t *testing.T, bundle, rendered, origin string) (*gentianov1alpha1.ComponentProfile, []client.Object) {
	t.Helper()
	var profile *gentianov1alpha1.ComponentProfile
	var objects []client.Object
	reader := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(rendered)), 4096)
	for {
		var doc map[string]any
		if err := reader.Decode(&doc); err != nil {
			break
		}
		if doc == nil {
			continue
		}
		if doc["kind"] == "ComponentProfile" {
			// The API server's part: the schema's defaults.
			s, err := profileSchema()
			if err != nil {
				t.Fatal(err)
			}
			applyDefaults(doc, s)
			raw, _ := json.Marshal(doc)
			profile = &gentianov1alpha1.ComponentProfile{}
			if err := json.Unmarshal(raw, profile); err != nil {
				t.Fatal(err)
			}
			continue
		}
		u := &unstructured.Unstructured{Object: doc}
		if u.GetKind() == KindConfigMap || u.GetKind() == KindCustomization {
			u.SetNamespace(catalogueNamespace)
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["app.kubernetes.io/instance"] = "gentian-catalogue-dev"
		u.SetLabels(labels)
		objects = append(objects, u)
	}
	if profile == nil {
		t.Fatal("the bundle holds no profile")
	}
	if profile.Annotations == nil {
		profile.Annotations = map[string]string{}
	}
	profile.Annotations[Annotation] = Encode([]byte(bundle))
	if origin != "" {
		profile.Annotations[OriginAnnotation] = origin
	}
	return profile, objects
}

func cluster(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func find(t *testing.T, objects []client.Object, kind string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objects {
		if u := o.(*unstructured.Unstructured); u.GetKind() == kind {
			return u
		}
	}
	t.Fatalf("no %s among the companions", kind)
	return nil
}

func set(t *testing.T, u *unstructured.Unstructured, value any, path ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, value, path...); err != nil {
		t.Fatal(err)
	}
}

func verifyOn(t *testing.T, profile *gentianov1alpha1.ComponentProfile, objects []client.Object, bundle string) *Refusal {
	t.Helper()
	refusal, err := VerifyOnCluster(context.Background(), cluster(objects...), catalogueNamespace, profile, Digest([]byte(bundle)))
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestABundleWhoseCompanionsAreAllThereAsItSaysIsVerified(t *testing.T) {
	profile, objects := applied(t, shopBundle, ClusterOrigin("main"))
	if r := verifyOn(t, profile, objects, shopBundle); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
	if got := OwnComposition(profile); got != "app-shop" {
		t.Fatalf("own composition: %q", got)
	}
}

// A profile materialised before bundles brought anything: its carrier holds
// the profile alone, no origin was recorded, and nothing beside it is read.
func TestTheOldCarrierIsStillRead(t *testing.T) {
	profile, _ := applied(t, wiki, "")
	if r := verifyOn(t, profile, nil, wiki); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
	if got := OwnComposition(profile); got != "" {
		t.Fatalf("own composition: %q", got)
	}
}

func TestAMissingCompanionHoldsTheRollout(t *testing.T) {
	for _, kind := range []string{KindComposition, KindConfigMap, KindCustomization, KindOIDCPacks} {
		profile, objects := applied(t, shopBundle, ClusterOrigin("main"))
		var rest []client.Object
		for _, o := range objects {
			if o.GetObjectKind().GroupVersionKind().Kind != kind {
				rest = append(rest, o)
			}
		}
		r := verifyOn(t, profile, rest, shopBundle)
		refused(t, "no "+kind, r, ReasonCompanionMissing, kind, "not on this cluster")
		if !r.Retry {
			t.Fatalf("no %s: a companion Argo CD has not applied yet is not looked for again", kind)
		}
	}
	// One in another namespace is not the bundle's.
	profile, objects := applied(t, shopBundle, ClusterOrigin("main"))
	find(t, objects, KindConfigMap).SetNamespace("tenant-acme")
	refused(t, "a ConfigMap elsewhere", verifyOn(t, profile, objects, shopBundle), ReasonCompanionMissing, catalogueNamespace)
}

func TestACompanionThatIsNotWhatTheBundleSaysHoldsTheRollout(t *testing.T) {
	cases := []struct {
		what   string
		kind   string
		change func(u *unstructured.Unstructured)
		says   string
	}{
		{"a template edited in the cluster", KindComposition, func(u *unstructured.Unstructured) {
			steps, _, _ := unstructured.NestedSlice(u.Object, "spec", "pipeline")
			set(t, &unstructured.Unstructured{Object: steps[0].(map[string]any)}, "{{ \"something else\" }}\n", "input", "inline", "template")
			set(t, u, steps, "spec", "pipeline")
		}, "spec.pipeline[0].input"},
		{"a field added to what a step is given", KindComposition, func(u *unstructured.Unstructured) {
			steps, _, _ := unstructured.NestedSlice(u.Object, "spec", "pipeline")
			set(t, &unstructured.Unstructured{Object: steps[0].(map[string]any)}, "<<", "input", "delims", "left")
			set(t, u, steps, "spec", "pipeline")
		}, "spec.pipeline[0].input"},
		{"a step added", KindComposition, func(u *unstructured.Unstructured) {
			steps, _, _ := unstructured.NestedSlice(u.Object, "spec", "pipeline")
			steps = append(steps, map[string]any{"step": "more", "functionRef": map[string]any{"name": "function-anything"}})
			set(t, u, steps, "spec", "pipeline")
		}, "spec.pipeline"},
		{"a step's function swapped", KindComposition, func(u *unstructured.Unstructured) {
			steps, _, _ := unstructured.NestedSlice(u.Object, "spec", "pipeline")
			set(t, &unstructured.Unstructured{Object: steps[0].(map[string]any)}, "function-other", "functionRef", "name")
			set(t, u, steps, "spec", "pipeline")
		}, "spec.pipeline[0].functionRef.name"},
		{"a Composition made another kind's", KindComposition, func(u *unstructured.Unstructured) {
			set(t, u, "XTenant", "spec", "compositeTypeRef", "kind")
		}, "spec.compositeTypeRef.kind"},
		{"a Composition relabelled", KindComposition, func(u *unstructured.Unstructured) {
			set(t, u, "wiki", "metadata", "labels", ProfileLabel)
		}, "label " + ProfileLabel},
		{"a page edited", KindConfigMap, func(u *unstructured.Unstructured) {
			set(t, u, "<html>hello</html>", "data", "sso.html")
		}, "data"},
		{"a file added to a ConfigMap", KindConfigMap, func(u *unstructured.Unstructured) {
			set(t, u, "x", "data", "more.html")
		}, "data"},
		{"binary data added", KindConfigMap, func(u *unstructured.Unstructured) {
			set(t, u, "YQ==", "binaryData", "a")
		}, "binaryData"},
		{"a ConfigMap passed off as the cluster's configuration", KindConfigMap, func(u *unstructured.Unstructured) {
			set(t, u, "cluster-config", "metadata", "labels", "gentianos.io/config-type")
		}, "label gentianos.io/config-type"},
		{"a pack added for another client", KindOIDCPacks, func(u *unstructured.Unstructured) {
			set(t, u, map[string]any{"serviceClient": true}, "spec", "packs", "gentian-dovecot")
		}, "spec.packs"},
		{"a pack made a public client", KindOIDCPacks, func(u *unstructured.Unstructured) {
			set(t, u, true, "spec", "packs", "gentian-shop", "publicClient")
		}, "spec.packs"},
		{"a group added", KindOIDCPacks, func(u *unstructured.Unstructured) {
			set(t, u, []any{"Admins"}, "spec", "extraManagedByAttributeGroups")
		}, "spec.extraManagedByAttributeGroups"},
		{"a record's review moved", KindCustomization, func(u *unstructured.Unstructured) {
			set(t, u, "2031-01-01", "spec", "reviewBy")
		}, "spec.reviewBy"},
		{"a field added to a record", KindCustomization, func(u *unstructured.Unstructured) {
			set(t, u, "low", "spec", "upgradeRisk")
		}, "spec.upgradeRisk"},
	}
	for _, c := range cases {
		profile, objects := applied(t, shopBundle, ClusterOrigin("main"))
		c.change(find(t, objects, c.kind))
		r := verifyOn(t, profile, objects, shopBundle)
		refused(t, c.what, r, ReasonCompanionMismatch, c.kind, c.says)
	}
}

// What the cluster adds to an object and the bundle could not have said is
// not a difference: what the API server and Argo CD write on every object,
// a status, and -- for the one kind compared without a schema -- a field
// filled in beside those the bundle states.
func TestWhatTheClusterAddsIsNotADifference(t *testing.T) {
	profile, objects := applied(t, shopBundle, ClusterOrigin("main"))
	for _, o := range objects {
		u := o.(*unstructured.Unstructured)
		u.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "gentian-catalogue-dev:/" + u.GetName()})
		u.SetFinalizers([]string{"example.org/kept"})
		set(t, u, map[string]any{"observed": true}, "status")
	}
	composition := find(t, objects, KindComposition)
	// As Crossplane's schema fills them in.
	set(t, composition, map[string]any{"name": "default"}, "spec", "publishConnectionDetailsWithStoreConfigRef")
	set(t, composition, "crossplane-system", "spec", "writeConnectionSecretsToNamespace")
	if r := verifyOn(t, profile, objects, shopBundle); r != nil {
		t.Fatalf("%s: %s", r.Reason, r.Message)
	}
}

// The carrier is the cluster's word on what the bundle is, and it is not
// taken: one that hashes as pinned and holds what a bundle may not is not
// rolled out, and neither is one whose companions no origin vouches for.
func TestABundleInTheClusterIsHeldToTheListAgain(t *testing.T) {
	profile, objects := applied(t, shopBundle, TenantOrigin("acme", "own"))
	refused(t, "companions on a tenant's profile", verifyOn(t, profile, objects, shopBundle), ReasonRefused, "only a catalogue of the whole cluster")

	profile, objects = applied(t, shopBundle, "")
	refused(t, "companions with no origin", verifyOn(t, profile, objects, shopBundle), ReasonRefused, "only a catalogue of the whole cluster")
	if got := OwnComposition(profile); got != "" {
		t.Fatalf("a Composition no cluster catalogue brought is the profile's own: %q", got)
	}

	secret := stream(shop, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: shop\n")
	profile, objects = applied(t, secret, ClusterOrigin("main"))
	refused(t, "a Secret", verifyOn(t, profile, objects, secret), ReasonRefused, "not a kind a bundle may hold")
}

// The largest bundle that is carried still fits where it is carried: the
// profile's annotations, which the API server bounds at 256 KiB in total.
func TestTheLargestBundleFitsItsCarrier(t *testing.T) {
	const annotationsLimit = 256 << 10
	carried := len(Annotation) + len(Encode(make([]byte, MaxBytes)))
	origin := len(OriginAnnotation) + len(TenantOrigin(strings.Repeat("t", 63), strings.Repeat("s", 63)))
	if room := annotationsLimit - carried - origin; room < 8<<10 {
		t.Fatalf("a bundle of MaxBytes leaves %d bytes for a profile's own annotations", room)
	}
}

// The bundles gentian-apps builds, as its build script wrote them
// (testdata/bundles/README.md says from which commit). Each has to be a
// bundle the director accepts from a cluster catalogue and refuses from a
// tenant's when it brings anything, and -- applied to a cluster -- has to
// verify whole.
//
// Applied means what `kubectl kustomize` prints for the catalogue directory,
// where kubectl is to be had: that is the reading the cluster receives.
func TestTheBuiltBundlesAreAcceptedAndVerify(t *testing.T) {
	want := map[string]string{
		"activepieces-me":       "ConfigMap activepieces-me.sign-in-handler, Customization activepieces-me.nginx-sso-routing, Customization activepieces-me.sign-in-sidecar",
		"docmost-ce":            "ConfigMap docmost-ce.sign-in-handler, Customization docmost-ce.sign-in-sidecar",
		"element-ce":            "Composition app-element-ce, ConfigMap element-ce.jitsi-oidc-overlays, OIDCPackCatalog element-ce-oidc",
		"nextcloud-base-ce":     "ConfigMap nextcloud-base-ce.portal-bridge-sso, OIDCPackCatalog nextcloud-base-ce-oidc",
		"nextcloud-base-od":     "OIDCPackCatalog nextcloud-base-od-oidc",
		"nextcloud-calendar-ce": "",
		"odoo-base-ce":          "Composition app-odoo-base-ce",
		"openproject-ce":        "Composition app-openproject-ce, ConfigMap openproject-ce.portal-bridge, OIDCPackCatalog openproject-ce-oidc",
		"xwiki-ce":              "OIDCPackCatalog xwiki-ce-oidc",
	}
	files, err := filepath.Glob(filepath.Join("testdata", "bundles", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(want) {
		t.Fatalf("%d bundles in testdata, %d expected", len(files), len(want))
	}
	kubectl, _ := exec.LookPath("kubectl")
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > MaxBytes {
				t.Fatalf("%d bytes, and the carrier takes %d", len(raw), MaxBytes)
			}
			b, err := Check(raw, name, ClusterOrigin("gentian"))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range b.Companions {
				got = append(got, c.String())
			}
			if strings.Join(got, ", ") != want[name] {
				t.Fatalf("companions: %s", strings.Join(got, ", "))
			}
			_, err = Check(raw, name, TenantOrigin("acme", "own"))
			if (err != nil) != (len(b.Companions) > 0) {
				t.Fatalf("from a tenant's own catalogue: %v", err)
			}

			// Without kubectl, the documents as this package reads them: a
			// bare date is a timestamp once kustomize has printed it, and
			// no other reader here knows that.
			var rendered string
			docs, err := documents(raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, doc := range docs {
				out, err := yaml.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				rendered += "---\n" + string(out)
			}
			if kubectl != "" {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, name+".yaml"), raw, 0o644); err != nil {
					t.Fatal(err)
				}
				listing := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - " + name + ".yaml\n"
				if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(listing), 0o644); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command(kubectl, "kustomize", dir).Output()
				if err != nil {
					t.Fatalf("kubectl kustomize: %v", err)
				}
				rendered = string(out)
			}
			profile, objects := appliedFrom(t, string(raw), rendered, ClusterOrigin("gentian"))
			if len(objects) != len(b.Companions) {
				t.Fatalf("%d objects applied for %d companions", len(objects), len(b.Companions))
			}
			if r := verifyOn(t, profile, objects, string(raw)); r != nil {
				t.Fatalf("%s: %s", r.Reason, r.Message)
			}
			if own := OwnComposition(profile); own != b.Composition() || (own != "" && own != profile.Spec.Package.Composition) {
				t.Fatalf("own composition %q, the bundle's %q, the profile's %q", own, b.Composition(), profile.Spec.Package.Composition)
			}
		})
	}
}
