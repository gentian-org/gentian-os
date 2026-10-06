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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func uninstallScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func directRelease(name string, labels map[string]string) *unstructured.Unstructured {
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	release.SetName(name)
	release.SetLabels(labels)
	return release
}

func releaseExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	err := c.Get(context.Background(), types.NamespacedName{Name: name}, release)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// A chart delivered directly is a cluster-scoped Release nothing else deletes:
// not the component's namespace, and not an owner reference, which a
// namespaced object cannot hold over it. So the component's finalizer does,
// and the component is not gone before its release is -- which is what a
// purge waits for. A Release that merely shares the name is somebody else's.
func TestUninstallingADirectlyDeliveredChartRemovesItsRelease(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	deleted := metav1.NewTime(time.Now())
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: "desktop", Namespace: "tenant-acme",
		Finalizers: []string{componentFinalizer}, DeletionTimestamp: &deleted,
	}}
	other := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: "console", Namespace: "tenant-globex",
		Finalizers: []string{componentFinalizer}, DeletionTimestamp: &deleted,
	}}
	own := directRelease(releaseName(comp), componentLabels(comp))
	// Held by a finalizer, as provider-helm holds a Release while it runs
	// `helm uninstall`.
	own.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	foreign := directRelease(releaseName(other), map[string]string{componentLabel: "something-else"})
	neighbour := directRelease("tenant-acme-wiki", componentLabels(&gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-acme"}}))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, other, own, foreign, neighbour).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(comp)})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("the component let go while its release was still being uninstalled")
	}
	held := &unstructured.Unstructured{}
	held.SetGroupVersionKind(helmReleaseGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: releaseName(comp)}, held); err != nil {
		t.Fatal(err)
	}
	if held.GetDeletionTimestamp().IsZero() {
		t.Fatal("the finalizer did not delete the release the component created")
	}
	still := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), still); err != nil {
		t.Fatalf("the component is gone before its release is: %v", err)
	}

	// The provider finishes the uninstall.
	held.SetFinalizers(nil)
	if err := c.Update(ctx, held); err != nil {
		t.Fatal(err)
	}
	if releaseExists(t, c, releaseName(comp)) {
		t.Fatal("the release outlived its last finalizer")
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(comp)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), still); !apierrors.IsNotFound(err) {
		t.Fatalf("the component stayed after its release was gone: %v", err)
	}

	// A release of the same name that this reconciler did not make for this
	// component is left alone, and does not hold the component.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)}); err != nil {
		t.Fatal(err)
	}
	if !releaseExists(t, c, releaseName(other)) {
		t.Fatal("a release this component did not create was deleted by its name alone")
	}
	if !releaseExists(t, c, "tenant-acme-wiki") {
		t.Fatal("uninstalling one component deleted another's release")
	}
}

// Uninstalling keeps the app's files: the release a component is delivered as
// is rendered with a rule that marks every claim the chart templates for Helm
// to leave in place, and it cannot be installed without that rule.
func TestADirectlyDeliveredChartKeepsItsVolumesOnUninstall(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "desktop", "tenant-acme", "uid-desktop"

	profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	if _, _, err := r.ensureRelease(ctx, comp, profile, map[string]interface{}{}, pullSecrets{}); err != nil {
		t.Fatal(err)
	}

	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-acme-desktop"}, release); err != nil {
		t.Fatalf("the release is not named after the tenant and the component: %v", err)
	}
	patches, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "patchesFrom")
	if len(patches) != 1 {
		t.Fatalf("patchesFrom = %v", patches)
	}
	ref, _, _ := unstructured.NestedMap(patches[0].(map[string]interface{}), "configMapKeyRef")
	if ref["name"] != "desktop-keep-volumes" || ref["namespace"] != "tenant-acme" || ref["key"] != "patch.yaml" || ref["optional"] != false {
		t.Fatalf("the release reads its keep rule from %v", ref)
	}

	rule := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: "desktop-keep-volumes", Namespace: "tenant-acme"}, rule); err != nil {
		t.Fatalf("the rule the release names does not exist: %v", err)
	}
	text := rule.Data["patch.yaml"]
	for _, want := range []string{"kind: PersistentVolumeClaim", "helm.sh/resource-policy: keep"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the rule does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "name: desktop") {
		t.Fatalf("the rule names one claim; it has to match every claim the chart renders:\n%s", text)
	}
	if !ownedBy(rule, comp) {
		t.Fatal("the rule is not the component's, and would outlive it")
	}

	// A second pass changes nothing and finds nothing to correct.
	before := release.GetResourceVersion()
	if _, _, err := r.ensureRelease(ctx, comp, profile, map[string]interface{}{}, pullSecrets{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-acme-desktop"}, release); err != nil {
		t.Fatal(err)
	}
	if release.GetResourceVersion() != before {
		t.Fatal("the release was rewritten on a pass that had nothing to change")
	}
}
