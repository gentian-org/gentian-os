/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package composed

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func managed(kind int, name, claimNS, claim, composite string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(Kinds[kind])
	u.SetName(name)
	u.SetLabels(map[string]string{ClaimNamespaceLabel: claimNS, ClaimNameLabel: claim, CompositeLabel: composite})
	u.SetAnnotations(annotations)
	_ = unstructured.SetNestedField(u.Object, "Delete", "spec", "deletionPolicy")
	return u
}

func ownedBy(u *unstructured.Unstructured, kind string) *unstructured.Unstructured {
	yes := true
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "gentianos.io/v1alpha1", Kind: kind, Name: u.GetLabels()[CompositeLabel], UID: "u", Controller: &yes}})
	return u
}

func xapp(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(XAppGVK)
	u.SetName(name)
	return u
}

func world(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).Build()
}

func get(t *testing.T, c client.Client, kind int, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(Kinds[kind])
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, u); err != nil {
		return nil
	}
	return u
}

const release, object = 0, 1

// The Release of a composite that is gone is a leftover nothing else will
// remove; one of the current composite is not a leftover at all, and one of
// a composite still there is that composite's to remove.
func TestFindTellsLeftoversFromTheCurrentComposite(t *testing.T) {
	c := world(t,
		xapp("wiki-new"), xapp("wiki-going"),
		managed(release, "wiki-old-release", "tenant-demo", "wiki", "wiki-old", nil),
		managed(release, "wiki-new-release", "tenant-demo", "wiki", "wiki-new", nil),
		managed(object, "wiki-going-secret", "tenant-demo", "wiki", "wiki-going", nil),
		managed(release, "drive-old-release", "tenant-demo", "drive", "drive-old", nil),
		managed(release, "wiki-elsewhere", "tenant-other", "wiki", "wiki-x", nil),
		ownedBy(managed(object, "tenant-thing", "tenant-demo", "wiki", "demo-xt", nil), "XTenant"),
		ownedBy(managed(object, "wiki-old-owned", "tenant-demo", "wiki", "wiki-old", nil), "XApp"),
	)
	got, err := Find(context.Background(), c, "tenant-demo", "wiki", "wiki-new")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("leftovers = %v", got)
	}
	byName := map[string]Leftover{}
	for _, l := range got {
		byName[l.Object.GetName()] = l
	}
	if l, ok := byName["wiki-old-release"]; !ok || !l.Orphaned {
		t.Errorf("the Release of the gone composite: %+v", l)
	}
	if _, ok := byName["tenant-thing"]; ok {
		t.Error("a resource another kind of composite controls was taken for the app's")
	}
	if l, ok := byName["wiki-old-owned"]; !ok || !l.Orphaned {
		t.Errorf("a resource still pointing at its gone XApp: %+v", l)
	}
	if l, ok := byName["wiki-going-secret"]; !ok || l.Orphaned {
		t.Errorf("the Object of the composite still there: %+v", l)
	}

	// Before a claim exists every composed resource of it is a leftover, and
	// "" finds those of every claim in the namespace.
	if got, _ := Find(context.Background(), c, "tenant-demo", "wiki", ""); len(got) != 4 {
		t.Errorf("without a current composite: %v", got)
	}
	if got, _ := Find(context.Background(), c, "tenant-demo", "", ""); len(got) != 5 {
		t.Errorf("every claim of the namespace: %v", got)
	}
}

// An orphan stuck on an unfinished create is unblocked and deleted, with its
// own deletion policy when nothing manages its release any more, and with
// Orphan when the app's current composite manages the same one. A leftover
// whose composite is still there is not touched.
func TestRemoveUnblocksAndDeletesOnlyOrphans(t *testing.T) {
	pending := map[string]string{"crossplane.io/external-create-pending": "2026-10-26T14:44:10Z", "keep": "me"}
	for _, keepExternal := range []bool{false, true} {
		c := world(t, xapp("wiki-going"),
			managed(release, "wiki-old-release", "tenant-demo", "wiki", "wiki-old", pending),
			managed(object, "wiki-going-secret", "tenant-demo", "wiki", "wiki-going", nil))
		// A finalizer, as the provider's, so the deletion can be looked at.
		stuck := get(t, c, release, "wiki-old-release")
		stuck.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
		if err := c.Update(context.Background(), stuck); err != nil {
			t.Fatal(err)
		}
		leftovers, err := Find(context.Background(), c, "tenant-demo", "wiki", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range leftovers {
			acted, err := Remove(context.Background(), c, l, keepExternal)
			if err != nil {
				t.Fatal(err)
			}
			if acted != l.Orphaned {
				t.Errorf("%s: acted = %v", l, acted)
			}
		}
		rel := get(t, c, release, "wiki-old-release")
		if rel == nil || rel.GetDeletionTimestamp() == nil {
			t.Fatalf("keepExternal=%v: the orphaned Release was not deleted: %v", keepExternal, rel)
		}
		if _, still := rel.GetAnnotations()["crossplane.io/external-create-pending"]; still || rel.GetAnnotations()["keep"] != "me" {
			t.Errorf("annotations = %v", rel.GetAnnotations())
		}
		policy, _, _ := unstructured.NestedString(rel.Object, "spec", "deletionPolicy")
		if want := map[bool]string{false: "Delete", true: "Orphan"}[keepExternal]; policy != want {
			t.Errorf("keepExternal=%v: deletionPolicy = %s, want %s", keepExternal, policy, want)
		}
		if obj := get(t, c, object, "wiki-going-secret"); obj == nil || obj.GetDeletionTimestamp() != nil {
			t.Errorf("the Object of a composite still there was touched: %v", obj)
		}
	}
}
