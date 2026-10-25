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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The set only grows by itself: an engine nothing declares any more stays
// wanted, a purge asked for while something declares it is refused and
// names who, and one asked for while nothing does is written for the
// composition to act on.
func TestTheEngineSetIsLatchedAndAPurgeRefusesWhileDeclared(t *testing.T) {
	declared := map[string][]string{EngineMariaDB: {"tenant-a/shop"}}
	got, refused := engineSet(map[string]string{
		EngineCache:                              engineWanted,
		enginePurgeRequestPrefix + EngineMariaDB: "yes",
	}, declared, declared)
	want := map[string]string{EngineMariaDB: engineWanted, EngineCache: engineWanted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("set = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(refused, map[string][]string{EngineMariaDB: {"tenant-a/shop"}}) {
		t.Fatalf("refused = %v", refused)
	}

	got, refused = engineSet(map[string]string{
		EngineCache:                            engineWanted,
		enginePurgeRequestPrefix + EngineCache: "yes",
		// A request for an engine the set never held does nothing.
		enginePurgeRequestPrefix + "mariadb": "yes",
	}, nil, nil)
	if !reflect.DeepEqual(got, map[string]string{EngineCache: enginePurge}) || len(refused) != 0 {
		t.Fatalf("set = %v refused = %v", got, refused)
	}

	// Declared again after a purge, it is wanted again.
	wiki := map[string][]string{EngineCache: {"tenant-b/wiki"}}
	got, _ = engineSet(map[string]string{EngineCache: enginePurge}, wiki, wiki)
	if got[EngineCache] != engineWanted {
		t.Fatalf("set = %v", got)
	}

	// Data an uninstalled app left on the engine holds it as well.
	retained := map[string][]string{EngineCache: {"demo: wiki (recorded)"}}
	got, refused = engineSet(map[string]string{EngineCache: engineWanted, enginePurgeRequestPrefix + EngineCache: "yes"}, nil, retained)
	if got[EngineCache] != engineWanted || len(refused[EngineCache]) != 1 {
		t.Fatalf("set = %v refused = %v", got, refused)
	}
}

func systemEnginesScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := componentDatabaseScheme(t)
	s.AddKnownTypeWithName(helmReleaseGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(helmReleaseGVK.GroupVersion().WithKind("ReleaseList"), &unstructured.UnstructuredList{})
	return s
}

func engineReleaseObject(name, namespace string, ready bool) *unstructured.Unstructured {
	rel := &unstructured.Unstructured{}
	rel.SetGroupVersionKind(helmReleaseGVK)
	rel.SetName(name)
	_ = unstructured.SetNestedField(rel.Object, namespace, "spec", "forProvider", "namespace")
	if ready {
		_ = unstructured.SetNestedSlice(rel.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	}
	return rel
}

// The operator writes the set from the components it finds, labelled for
// the composition, and a component that declares an engine waits until the
// engine's Release is Ready.
func TestTheEngineSetFollowsTheComponentsAndTheyWaitForTheEngine(t *testing.T) {
	ctx := context.Background()
	shop := storesProfile("shop", gentianov1alpha1.DatabaseEngineMariaDB, false, false)
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "tenant-a"},
		Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: "shop"}},
	}
	c := fake.NewClientBuilder().WithScheme(systemEnginesScheme(t)).WithObjects(shop, comp).Build()
	r := &SystemEnginesReconciler{Client: c, Namespace: "kernel-control"}
	if _, err := r.Reconcile(ctx, systemEnginesRequest); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: systemEnginesConfigMap, Namespace: "kernel-control"}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels["gentianos.io/config-type"] != systemEnginesConfigType || cm.Data[EngineMariaDB] != engineWanted || len(cm.Data) != 1 {
		t.Fatalf("set = %v labels = %v", cm.Data, cm.Labels)
	}

	if engine, err := systemEngineWaiting(ctx, c, shop); err != nil || engine != EngineMariaDB {
		t.Fatalf("waiting = %q, %v; want mariadb", engine, err)
	}
	if err := c.Create(ctx, engineReleaseObject("dev-mariadb", "system-mariadb", true)); err != nil {
		t.Fatal(err)
	}
	if engine, err := systemEngineWaiting(ctx, c, shop); err != nil || engine != "" {
		t.Fatalf("waiting = %q, %v; want none once the Release is Ready", engine, err)
	}

	// The component goes; the engine stays wanted.
	if err := c.Delete(ctx, comp); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, systemEnginesRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[EngineMariaDB] != engineWanted {
		t.Fatalf("the engine left the set with its last component: %v", cm.Data)
	}
}

// Once a purged engine's Release is gone, its volumes go too; while the
// Release is still there they are left alone.
func TestAPurgedEngineLosesItsVolumesOnlyOnceItsReleaseIsGone(t *testing.T) {
	ctx := context.Background()
	set := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"},
		Data:       map[string]string{EngineCache: enginePurge},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "redis-data-0", Namespace: "system-cache"}}
	rel := engineReleaseObject("dev-redis", "system-cache", true)
	c := fake.NewClientBuilder().WithScheme(systemEnginesScheme(t)).WithObjects(set, pvc, rel).Build()
	r := &SystemEnginesReconciler{Client: c, Namespace: "kernel-control"}
	res, err := r.Reconcile(ctx, systemEnginesRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("no requeue while the purged engine's Release is still there")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("the volume went while the Release was there: %v", err)
	}
	if err := c.Delete(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, systemEnginesRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err == nil {
		t.Fatal("the purged engine's volume is still there")
	}
}
