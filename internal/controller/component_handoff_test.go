/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/composed"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// handoffHarness is the wiki of tenant acme as the app Composition installed
// it: its App claim, controlled by the Component, names the composite
// wiki-x1, which composed a Release, a Keycloak Client and an Object wrapping
// the release's keep rule -- each owned by the composite and labeled as
// Crossplane labels what a claim's composite composes.
func handoffHarness(t *testing.T, objects ...client.Object) *digestHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	tenant := acmeTenantFixture()
	profile := materialised(t, wikiBundle)
	comp := componentFor("wiki", tenantNamespaceName(tenant))
	comp.UID = "uid-wiki"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(objects, tenant.DeepCopy(), profile, comp)...).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	recorder := record.NewFakeRecorder(16)
	return &digestHarness{t: t, c: c, comp: comp, recorder: recorder,
		r: &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", Recorder: recorder}}
}

const handoffXR = "wiki-x1"

// composedBy marks an object as one the composite composed for the wiki's
// claim.
func composedBy(obj *unstructured.Unstructured) *unstructured.Unstructured {
	controller := true
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: composed.XAppGVK.GroupVersion().String(), Kind: composed.XAppGVK.Kind,
		Name: handoffXR, UID: "uid-xr", Controller: &controller,
	}})
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[composed.CompositeLabel] = handoffXR
	labels[composed.ClaimNameLabel] = "wiki"
	labels[composed.ClaimNamespaceLabel] = "tenant-acme"
	obj.SetLabels(labels)
	return obj
}

func handoffObjects(comp *gentianov1alpha1.Component) []client.Object {
	claim := appClaimOf(comp, "Component", comp.Name, true)
	_ = unstructured.SetNestedField(claim.Object, handoffXR, "spec", "resourceRef", "name")

	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	release.SetName(handoffXR + composedReleaseSuffix)
	release.SetAnnotations(map[string]string{"crossplane.io/external-name": "wiki" + composedReleaseSuffix})
	_ = unstructured.SetNestedField(release.Object, "tenant-acme", "spec", "forProvider", "namespace")
	composedBy(release)

	keycloak := &unstructured.Unstructured{}
	keycloak.SetAPIVersion("openidclient.keycloak.crossplane.io/v1alpha1")
	keycloak.SetKind("Client")
	keycloak.SetName("acme-wiki-keycloak-client")
	composedBy(keycloak)

	keep := &unstructured.Unstructured{}
	keep.SetGroupVersionKind(objectWrapperGVK)
	keep.SetName(handoffXR + "-keep-volumes")
	_ = unstructured.SetNestedField(keep.Object, "Delete", "spec", "deletionPolicy")
	_ = unstructured.SetNestedMap(keep.Object, map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "wiki-release-keep-volumes", "namespace": "tenant-acme"},
	}, "spec", "forProvider", "manifest")
	composedBy(keep)

	wrapped := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "wiki-release-keep-volumes", Namespace: "tenant-acme",
		Labels: map[string]string{composed.CompositeLabel: handoffXR},
	}}

	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(composed.XAppGVK)
	xr.SetName(handoffXR)
	xr.SetUID("uid-xr")
	var refs []interface{}
	for _, obj := range []*unstructured.Unstructured{release, keycloak, keep} {
		refs = append(refs, map[string]interface{}{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "name": obj.GetName()})
	}
	_ = unstructured.SetNestedSlice(xr.Object, refs, "spec", "resourceRefs")
	return []client.Object{claim, release, keycloak, keep, wrapped, xr}
}

func (h *digestHarness) live(gvkOf *unstructured.Unstructured, name, namespace string) *unstructured.Unstructured {
	h.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkOf.GroupVersionKind())
	err := h.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return obj
}

func ownedByXR(obj *unstructured.Unstructured) bool {
	for _, o := range obj.GetOwnerReferences() {
		if o.UID == "uid-xr" {
			return true
		}
	}
	return false
}

// An app the Composition installed is taken over in place, pass by pass. The
// composite is paused first, so that it puts nothing back; then everything it
// composed loses its owner reference, a wrapper is set to orphan what it
// wraps and what it wraps is marked the component's, and the claim is
// deleted. The component records the composite it was installed as. Once
// the composite is gone, its wrapper goes without the ConfigMap, and the
// release is rendered under the Release object that is there -- the same
// object, now the component's, never a second Release beside it installing
// the same Helm release again.
func TestAnAppTheCompositionInstalledIsTakenOverInPlace(t *testing.T) {
	ctx := context.Background()
	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-wiki"
	objs := handoffObjects(comp)
	claim, release, keycloak, keep, xr := objs[0].(*unstructured.Unstructured), objs[1].(*unstructured.Unstructured),
		objs[2].(*unstructured.Unstructured), objs[3].(*unstructured.Unstructured), objs[5].(*unstructured.Unstructured)
	h := handoffHarness(t, objs...)

	// The first pass pauses the composite and takes nothing yet.
	got := h.reconcile()
	if got.Annotations[compositeAnnotation] != handoffXR {
		t.Fatalf("annotations = %v, want the composite recorded", got.Annotations)
	}
	if h.live(xr, handoffXR, "").GetAnnotations()[pausedAnnotation] != "true" {
		t.Fatal("the composite was not paused first")
	}
	if !ownedByXR(h.live(release, release.GetName(), "")) {
		t.Fatal("a resource was taken before the composite was paused")
	}
	if ready := componentReadyCondition(got); ready == nil || !strings.Contains(ready.Message, "pausing the composite") {
		t.Fatalf("condition = %+v", ready)
	}

	// Nothing is taken while Crossplane has not said it stopped.
	h.reconcile()
	if !ownedByXR(h.live(release, release.GetName(), "")) {
		t.Fatal("a resource was taken before Crossplane reported the composite paused")
	}
	paused := h.live(xr, handoffXR, "")
	_ = unstructured.SetNestedSlice(paused.Object, []interface{}{
		map[string]interface{}{"type": "Synced", "status": "False", "reason": "ReconcilePaused"},
	}, "status", "conditions")
	if err := h.c.Update(ctx, paused); err != nil {
		t.Fatal(err)
	}

	// The next takes everything over, into the component's inventory, and
	// deletes the claim.
	h.reconcile()
	if inv := strings.Join(takenOf(h.reconciledComponent()), " "); !strings.Contains(inv, release.GetName()) || !strings.Contains(inv, keycloak.GetName()) {
		t.Errorf("what was taken over is not written down: %s", inv)
	}
	for _, ref := range []*unstructured.Unstructured{release, keycloak, keep} {
		if ownedByXR(h.live(ref, ref.GetName(), "")) {
			t.Errorf("%s %s is still owned by the composite", ref.GetKind(), ref.GetName())
		}
	}
	if policy, _, _ := unstructured.NestedString(h.live(keep, keep.GetName(), "").Object, "spec", "deletionPolicy"); policy != "Orphan" {
		t.Errorf("the wrapper's deletionPolicy is %q, want Orphan", policy)
	}
	wrapped := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "wiki-release-keep-volumes", Namespace: "tenant-acme"}, wrapped); err != nil {
		t.Fatal(err)
	}
	for k, v := range componentLabels(h.comp) {
		if wrapped.Labels[k] != v {
			t.Errorf("the wrapped ConfigMap's labels %v lack %s=%s", wrapped.Labels, k, v)
		}
	}
	if h.live(claim, claim.GetName(), claim.GetNamespace()) != nil {
		t.Fatal("the claim was not deleted")
	}
	if h.live(xr, handoffXR, "") == nil {
		t.Fatal("the composite went before Crossplane deleted it")
	}

	// Crossplane deletes the composite behind the claim; the operator lets
	// it go, and Crossplane drops its finalizer.
	current := h.live(xr, handoffXR, "")
	current.SetFinalizers([]string{"composite.apiextensions.crossplane.io"})
	if err := h.c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	current = h.live(xr, handoffXR, "")
	if current == nil || current.GetAnnotations()[pausedAnnotation] == "true" {
		t.Fatalf("the composite being deleted was not let go: %v", current)
	}
	current.SetFinalizers(nil)
	if err := h.c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}

	// Nothing of the composite is left: the release is rendered.
	h.reconcileInstalled()
	if h.live(keep, keep.GetName(), "") != nil {
		t.Fatal("the composite's wrapper outlived it")
	}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "wiki-release-keep-volumes", Namespace: "tenant-acme"}, wrapped); err != nil {
		t.Fatalf("the wrapper took the ConfigMap it wrapped with it: %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseGVK.GroupVersion().WithKind("ReleaseList"))
	if err := h.c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		var names []string
		for _, r := range list.Items {
			names = append(names, r.GetName())
		}
		t.Fatalf("releases %v, want only the one taken over", names)
	}
	taken := list.Items[0]
	if taken.GetName() != release.GetName() || taken.GetAnnotations()["crossplane.io/external-name"] != "wiki"+composedReleaseSuffix {
		t.Fatalf("release %s (external-name %s), want %s installing wiki%s", taken.GetName(),
			taken.GetAnnotations()["crossplane.io/external-name"], release.GetName(), composedReleaseSuffix)
	}
	if version, _, _ := unstructured.NestedString(taken.Object, "spec", "forProvider", "chart", "version"); version != "1.0.0" {
		t.Fatalf("the release was not rendered over: chart version %q", version)
	}
	if taken.GetLabels()[componentLabel] != "wiki" {
		t.Fatalf("the release is not marked the component's: %v", taken.GetLabels())
	}
	inventoried := false
	for _, key := range inventoryOf(getComponent(t, h.c, h.comp)) {
		if key == inventoryKey(&taken) {
			inventoried = true
		}
	}
	if !inventoried {
		t.Fatal("the release taken over is not in the component's inventory, and would outlive it")
	}
}

// A profile that brings its own Composition is rendered by it only at the
// platform's trust: what a Composition renders is anything at all. Below it
// the install is held with CompositionRefused, and neither an App claim nor
// a release is made. At the platform's trust the claim is written.
func TestOnlyAPlatformProfileIsRenderedByItsOwnComposition(t *testing.T) {
	for _, c := range []struct {
		tier    string
		refused bool
	}{
		{"certified", true},
		{"platform", false},
	} {
		t.Run(c.tier, func(t *testing.T) {
			bundle := strings.Replace(shopWithComposition, "trustTier: certified", "trustTier: "+c.tier, 1)
			profile := materialised(t, bundle)
			profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
			h := startDigestHarness(t, profile, profilebundle.Digest([]byte(bundle)))
			// The Composition the bundle brings, as Argo CD applies it.
			own := yamlDocsOf(t, strings.SplitN(bundle, "---\n", 2)[1])
			if err := h.c.Create(context.Background(), own); err != nil {
				t.Fatal(err)
			}
			if appComposition(h.comp, profile) != "app-shop" {
				t.Fatal("the fixture is meant to bring its own, verified Composition")
			}
			ready := componentReadyCondition(h.reconcile())
			claim := &unstructured.Unstructured{}
			claim.SetGroupVersionKind(appClaimGVK)
			claimErr := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), claim)
			if !c.refused {
				if claimErr != nil {
					t.Fatalf("no App claim for a platform profile's own Composition: %v (condition %+v)", claimErr, ready)
				}
				return
			}
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonCompositionRefused {
				t.Fatalf("condition = %+v, want %s", ready, reasonCompositionRefused)
			}
			if !apierrors.IsNotFound(claimErr) {
				t.Fatalf("an App claim was written: %v", claimErr)
			}
			if h.releasedVersion() != "" {
				t.Fatal("a release was rendered for a profile whose Composition was refused")
			}
		})
	}
}

// yamlDocsOf reads one YAML document.
func yamlDocsOf(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

// reconciledComponent is the Component as stored now.
func (h *digestHarness) reconciledComponent() *gentianov1alpha1.Component {
	h.t.Helper()
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), comp); err != nil {
		h.t.Fatal(err)
	}
	return comp
}

// An app uninstalled while its handoff is under way is not left running: the
// handoff is finished for the removal, what the composite composed goes into
// the component's inventory, and goes with the component -- the release
// first, waited for. The composite is let go.
func TestAnAppUninstalledDuringItsHandoffIsRemoved(t *testing.T) {
	ctx := context.Background()
	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-wiki"
	objs := handoffObjects(comp)
	release, keycloak, xr := objs[1].(*unstructured.Unstructured), objs[2].(*unstructured.Unstructured), objs[5].(*unstructured.Unstructured)
	h := handoffHarness(t, objs...)
	h.reconcile() // the finalizer
	h.reconcile() // paused, nothing taken yet
	if h.live(xr, handoffXR, "").GetAnnotations()[pausedAnnotation] != "true" {
		t.Fatal("the composite was not paused")
	}

	stored := h.reconciledComponent()
	if err := h.c.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	// The composite is not being deleted yet: it stays paused, and its claim
	// is deleted, which is what deletes it.
	h.reconcile()
	live := h.live(xr, handoffXR, "")
	if live.GetAnnotations()[pausedAnnotation] != "true" {
		t.Fatal("a composite still claimed was let go, and would compose everything again")
	}
	if ownedByXR(h.live(release, release.GetName(), "")) {
		t.Fatal("the release is still the composite's")
	}
	if h.live(objs[0].(*unstructured.Unstructured), "wiki", "tenant-acme") != nil {
		t.Fatal("the claim was not deleted")
	}
	// Crossplane deletes the composite; the next pass lets it go and removes
	// what was taken over.
	live.SetFinalizers([]string{"composite.apiextensions.crossplane.io"})
	if err := h.c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(ctx, h.live(xr, handoffXR, "")); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if h.live(xr, handoffXR, "").GetAnnotations()[pausedAnnotation] == "true" {
		t.Error("the composite is still paused, and would never let go of its finalizer")
	}
	if got := h.live(keycloak, keycloak.GetName(), ""); got != nil && got.GetDeletionTimestamp().IsZero() {
		t.Error("the app's client at the realm was left behind")
	}
	if got := h.live(release, release.GetName(), ""); got != nil && got.GetDeletionTimestamp().IsZero() {
		t.Error("the app's release was left running")
	}
}

// What the composite watched without managing -- a Service the chart makes
// itself -- is not marked the component's: it would be owned by it and
// removed as an object no longer rendered.
func TestWhatTheCompositeOnlyWatchedIsNotTaken(t *testing.T) {
	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-wiki"
	objs := handoffObjects(comp)
	watcher := objs[3].(*unstructured.Unstructured)
	_ = unstructured.SetNestedStringSlice(watcher.Object, []string{"Observe"}, "spec", "managementPolicies")
	xr := objs[5].(*unstructured.Unstructured)
	_ = unstructured.SetNestedSlice(xr.Object, []interface{}{
		map[string]interface{}{"type": "Synced", "status": "False", "reason": "ReconcilePaused"},
	}, "status", "conditions")
	h := handoffHarness(t, objs...)
	h.reconcile()
	h.reconcile()
	h.reconcile()
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	live := h.live(cm, "wiki-release-keep-volumes", "tenant-acme")
	if live == nil {
		t.Fatal("what the composite watched is gone")
	}
	if metav1.GetControllerOf(live) != nil || live.GetLabels()[renderedLabel] != "" {
		t.Fatalf("what the composite only watched was taken: labels %v, owner %+v", live.GetLabels(), metav1.GetControllerOf(live))
	}
}

// A post-install job the Composition ran is taken over as it is: a Job runs
// what it was made with and cannot be changed, so only its labels and owner
// become the component's. Rewriting it was refused by the API server on
// every pass, and nothing after it was ever written.
func TestATakenOverJobIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-wiki"
	job := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]interface{}{"name": "wiki-post-install-abc", "namespace": "tenant-acme",
			"labels": map[string]interface{}{componentLabel: "wiki", tenantLabel: "acme"}},
		"spec": map[string]interface{}{"template": map[string]interface{}{"metadata": map[string]interface{}{
			"labels": map[string]interface{}{"job-name": "wiki-post-install-abc"}}}},
	}}
	h := handoffHarness(t, job)
	rendered := job.DeepCopy()
	_ = unstructured.SetNestedField(rendered.Object, map[string]interface{}{"spec": map[string]interface{}{"restartPolicy": "Never"}}, "spec", "template")
	rendered.SetLabels(nil)
	stored := h.reconciledComponent()
	if _, err := h.r.applyObject(ctx, stored, rendered); err != nil {
		t.Fatal(err)
	}
	live := h.live(job, job.GetName(), "tenant-acme")
	if got, _, _ := unstructured.NestedString(live.Object, "spec", "template", "metadata", "labels", "job-name"); got != "wiki-post-install-abc" {
		t.Errorf("the Job's template was rewritten: %v", live.Object["spec"])
	}
	if owner := metav1.GetControllerOf(live); owner == nil || owner.Name != "wiki" {
		t.Errorf("the Job is not the component's: %+v", owner)
	}
}
