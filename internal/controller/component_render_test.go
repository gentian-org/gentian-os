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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// markRenderedReady makes every object rendered for a component outside its
// namespace report ready, as its provider does once it exists: the realm's
// objects, the releases.
func markRenderedReady(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) {
	t.Helper()
	current := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), current); err != nil {
		t.Fatal(err)
	}
	for _, key := range inventoryOf(current) {
		ref, _ := inventoryObject(key)
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(ref.GroupVersionKind())
		if err := c.Get(context.Background(), types.NamespacedName{Name: ref.GetName(), Namespace: ref.GetNamespace()}, live); err != nil {
			continue
		}
		_ = unstructured.SetNestedSlice(live.Object, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True"},
			map[string]interface{}{"type": "Synced", "status": "True"},
		}, "status", "conditions")
		if err := c.Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
}

// What Helm deploys a release from is its chart and values: a change there is
// a new revision to wait for. Anything else written to the Release is not,
// and nothing waits for it.
func TestOnlyAChangeOfChartOrValuesAwaitsANewHelmRevision(t *testing.T) {
	release := func(version string, values map[string]interface{}, rollbackLimit int64) *unstructured.Unstructured {
		r := &unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{"forProvider": map[string]interface{}{
				"chart":         map[string]interface{}{"name": "wiki", "version": version},
				"values":        values,
				"rollbackLimit": rollbackLimit,
			}},
		}}
		r.SetGroupVersionKind(helmReleaseGVK)
		return r
	}
	live := release("1.0.0", map[string]interface{}{"replicas": int64(1), "theme": "dark"}, 3)
	// The provider fills in beside what is rendered of the chart.
	_ = unstructured.SetNestedField(live.Object, "https://charts.example", "spec", "forProvider", "chart", "repository")

	for what, c := range map[string]struct {
		desired *unstructured.Unstructured
		want    bool
	}{
		"the same":               {release("1.0.0", map[string]interface{}{"replicas": int64(1), "theme": "dark"}, 3), false},
		"another rollback limit": {release("1.0.0", map[string]interface{}{"replicas": int64(1), "theme": "dark"}, 5), false},
		"another chart version":  {release("2.0.0", map[string]interface{}{"replicas": int64(1), "theme": "dark"}, 3), true},
		"another value":          {release("1.0.0", map[string]interface{}{"replicas": int64(2), "theme": "dark"}, 3), true},
		"a value no longer said": {release("1.0.0", map[string]interface{}{"replicas": int64(1)}, 3), true},
		// May resolve to the same values: provider-helm then makes no revision.
		"a value moved into valuesFrom": {withValuesFrom(release("1.0.0", map[string]interface{}{"replicas": int64(1), "theme": "dark"}, 3)), false},
	} {
		if got := releaseDeploysChange(live, c.desired); got != c.want {
			t.Errorf("%s: deploys a change = %v, want %v", what, got, c.want)
		}
	}

	// Waited for until the provider reports a later revision than the one noted.
	live.SetAnnotations(map[string]string{releaseRevisionBeforeAnnotation: "3"})
	_ = unstructured.SetNestedField(live.Object, int64(3), "status", "atProvider", "revision")
	if releaseAwaitsRevision(live) == "" {
		t.Error("a release still at the revision before is taken as deployed")
	}
	_ = unstructured.SetNestedField(live.Object, int64(4), "status", "atProvider", "revision")
	if waiting := releaseAwaitsRevision(live); waiting != "" {
		t.Errorf("a release at a later revision still waits: %s", waiting)
	}
}

func withValuesFrom(r *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(r.Object, []interface{}{
		map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "imap", "key": "values.yaml", "optional": true}},
	}, "spec", "forProvider", "valuesFrom")
	return r
}

// A release that is not Ready says why, a Helm error before any revision
// among it, rather than that it waits for a revision.
func TestAFailedReleaseSaysWhyRatherThanThatItWaits(t *testing.T) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(helmReleaseGVK)
	live.SetName("tenant-acme-wiki")
	live.SetAnnotations(map[string]string{releaseRevisionBeforeAnnotation: "3"})
	_ = unstructured.SetNestedField(live.Object, int64(3), "status", "atProvider", "revision")
	_ = unstructured.SetNestedSlice(live.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "message": "template: wiki/templates/x.yaml: bad"},
	}, "status", "conditions")
	r := &ComponentReconciler{Client: fake.NewClientBuilder().WithObjects(live).Build()}
	_, msg, err := r.renderedReady(context.Background(), []*unstructured.Unstructured{live})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "template: wiki/templates/x.yaml: bad") {
		t.Errorf("message = %q, want the Helm error", msg)
	}
}
