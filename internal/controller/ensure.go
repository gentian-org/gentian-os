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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ensureObject creates desired, or applies mutate to the object already there
// and merge-patches it when mutate says something changed.
func ensureObject[T client.Object](ctx context.Context, c client.Client, desired T, mutate func(existing T) bool) error {
	existing := desired.DeepCopyObject().(T)
	err := c.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopyObject().(T))
	if !mutate(existing) {
		return nil
	}
	return c.Patch(ctx, existing, patch)
}

// ensureSpec creates desired, or patches the spec of the object already there
// when it is not desired's. What else desired says is written when it is
// created only.
func ensureSpec(ctx context.Context, c client.Client, desired *unstructured.Unstructured) error {
	return ensureObject(ctx, c, desired, func(existing *unstructured.Unstructured) bool {
		if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
			return false
		}
		existing.Object["spec"] = desired.Object["spec"]
		return true
	})
}
