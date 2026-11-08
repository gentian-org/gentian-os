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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// RecordedAuthorizationStore is the kernel store the operator recorded in
// namespace, "" when nothing is recorded there yet. A reader that cannot be
// asked is nothing recorded: the operator then establishes the store as on a
// first install and records it again.
func RecordedAuthorizationStore(ctx context.Context, c client.Reader, namespace string) string {
	if c == nil {
		return ""
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: authz.PinConfigMap, Namespace: namespace}, cm); err != nil {
		return ""
	}
	return cm.Data[authz.PinStoreKey]
}

// RecordAuthorizationStore writes the kernel store and model into the
// ConfigMap authz.PinConfigMap of each namespace.
//
// The control namespace holds the record every reader there takes its ids
// from. The edge namespace holds a copy for the ext-auth bouncer, which runs
// there with no ServiceAccount token: a pod reads a ConfigMap only from its
// own namespace, and the bouncer's route table reaches it the same way. Both
// are written from the one answer Bootstrap gave, so they cannot disagree.
func RecordAuthorizationStore(ctx context.Context, c client.Client, namespaces []string, storeID, modelID string) error {
	data := map[string]string{authz.PinStoreKey: storeID, authz.PinModelKey: modelID}
	for _, ns := range namespaces {
		existing := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{Name: authz.PinConfigMap, Namespace: ns}, existing)
		if errors.IsNotFound(err) {
			cm := &corev1.ConfigMap{Data: data}
			cm.Name, cm.Namespace = authz.PinConfigMap, ns
			cm.Labels = map[string]string{managedByLabel: managedByValue}
			if err := c.Create(ctx, cm); err != nil {
				return fmt.Errorf("record the authorization store in %s: %w", ns, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read the authorization store record in %s: %w", ns, err)
		}
		if equality.Semantic.DeepEqual(existing.Data, data) {
			continue
		}
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = data
		if err := c.Patch(ctx, existing, patch); err != nil {
			return fmt.Errorf("record the authorization store in %s: %w", ns, err)
		}
	}
	return nil
}
