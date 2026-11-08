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
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// ensureExternalSecret creates desired, or replaces the ExternalSecret there
// when its spec or labels are not desired's or owned says its owners are not.
//
// Only on a real difference. An unconditional update rewrote the object on
// every pass, and ESO writes it too: the two raced, the loser got a conflict,
// and the conflict was reported as a failure.
func ensureExternalSecret(ctx context.Context, c client.Client, desired *unstructured.Unstructured, owned func(existing *unstructured.Unstructured) bool) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(secrets.ExternalSecretGVK)
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("create ExternalSecret %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		return nil
	case err != nil:
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) &&
		equality.Semantic.DeepEqual(existing.GetLabels(), desired.GetLabels()) && owned(existing) {
		return nil
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := c.Update(ctx, desired); err != nil {
		return fmt.Errorf("update ExternalSecret %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
	}
	return nil
}
