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
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func bridgeAddonsFixture(addonsPath string) []client.Object {
	profile := func(name string, spec gentianov1alpha1.ComponentProfileSpec) *gentianov1alpha1.ComponentProfile {
		return &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	}
	addon := func(name, id string) *gentianov1alpha1.ComponentProfile {
		return profile(name, gentianov1alpha1.ComponentProfileSpec{Package: gentianov1alpha1.PackageSpec{
			Addon: &gentianov1alpha1.PackageAddon{ID: id, Of: "wiki-base"},
		}})
	}
	component := func(ns, name, profile string, addons ...string) *gentianov1alpha1.Component {
		return &gentianov1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: gentianov1alpha1.ComponentSpec{
				ProfileRef: gentianov1alpha1.ProfileRef{Name: profile}, Addons: addons,
			},
		}
	}
	return []client.Object{
		profile("wiki-base", gentianov1alpha1.ComponentProfileSpec{}),
		addon("wiki-pages", "pages"),
		addon("wiki-search", "search"),
		profile("wiki-tools", gentianov1alpha1.ComponentProfileSpec{Bridge: &gentianov1alpha1.BridgeSpec{
			App: &gentianov1alpha1.BridgedApp{Profile: "wiki-base"}, AddonsValuesPath: addonsPath,
			Ways: []gentianov1alpha1.BridgeWay{{Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
				Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolMCP,
				SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson}},
		}}),
		// The app in two tenants with different add-ons, and its bridge in one.
		component("tenant-a", "wiki-base", "wiki-base", "wiki-pages", "wiki-search"),
		component("tenant-b", "wiki-base", "wiki-base", "wiki-pages"),
		component("tenant-a", "wiki-tools", "wiki-tools"),
		component("tenant-c", "wiki-tools", "wiki-tools"),
		component("tenant-a", "other", "other-app"),
	}
}

func bridgeAddonsReconciler(t *testing.T, objs []client.Object) *ComponentReconciler {
	t.Helper()
	scheme := SchemeForTest(t)
	return &ComponentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Scheme: scheme}
}

// A bridge is told the add-ons of the app it connects in its own tenant, by
// their IDs, and an empty list where that app is not installed.
func TestBridgedAddonIDs(t *testing.T) {
	ctx := context.Background()
	r := bridgeAddonsReconciler(t, bridgeAddonsFixture("active"))
	ids := func(ns, name, profileName string) []string {
		t.Helper()
		comp := &gentianov1alpha1.Component{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, comp); err != nil {
			t.Fatal(err)
		}
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: profileName}, profile); err != nil {
			t.Fatal(err)
		}
		got, err := r.bridgedAddonIDs(ctx, comp, profile)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := ids("tenant-a", "wiki-tools", "wiki-tools"); !reflect.DeepEqual(got, []string{"pages", "search"}) {
		t.Fatalf("tenant-a's bridge is told %v, want [pages search]", got)
	}
	if got := ids("tenant-c", "wiki-tools", "wiki-tools"); got == nil || len(got) != 0 {
		t.Fatalf("a bridge whose app is not installed is told %#v, want an empty list", got)
	}
	if got := ids("tenant-a", "wiki-base", "wiki-base"); got != nil {
		t.Fatalf("an app is told %v, want nothing", got)
	}

	silent := bridgeAddonsReconciler(t, bridgeAddonsFixture(""))
	comp := &gentianov1alpha1.Component{}
	profile := &gentianov1alpha1.ComponentProfile{}
	_ = silent.Get(ctx, client.ObjectKey{Namespace: "tenant-a", Name: "wiki-tools"}, comp)
	_ = silent.Get(ctx, client.ObjectKey{Name: "wiki-tools"}, profile)
	if got, err := silent.bridgedAddonIDs(ctx, comp, profile); err != nil || got != nil {
		t.Fatalf("a bridge that does not ask is told %v (%v), want nothing", got, err)
	}
}

// A change to an app re-runs the bridges of that app in its tenant, and
// nothing else.
func TestBridgesOfApp(t *testing.T) {
	ctx := context.Background()
	objs := bridgeAddonsFixture("active")
	r := bridgeAddonsReconciler(t, objs)
	queued := func(obj client.Object) []string {
		t.Helper()
		q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
		defer q.ShutDown()
		bridgesOfApp(r.Client).Update(ctx, event.TypedUpdateEvent[client.Object]{ObjectOld: obj, ObjectNew: obj}, q)
		var out []string
		for q.Len() > 0 {
			item, _ := q.Get()
			out = append(out, item.Namespace+"/"+item.Name)
			q.Done(item)
		}
		sort.Strings(out)
		return out
	}
	byName := func(ns, name string) client.Object {
		for _, o := range objs {
			if o.GetNamespace() == ns && o.GetName() == name {
				return o
			}
		}
		t.Fatalf("no %s/%s in the fixture", ns, name)
		return nil
	}
	if got := queued(byName("tenant-a", "wiki-base")); !reflect.DeepEqual(got, []string{"tenant-a/wiki-tools"}) {
		t.Fatalf("a change to tenant-a's app queues %v, want [tenant-a/wiki-tools]", got)
	}
	if got := queued(byName("tenant-b", "wiki-base")); len(got) != 0 {
		t.Fatalf("a change in a tenant without the bridge queues %v, want nothing", got)
	}
	if got := queued(byName("tenant-a", "other")); len(got) != 0 {
		t.Fatalf("a change to an app no bridge connects queues %v, want nothing", got)
	}
}
