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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
