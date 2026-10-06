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

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// loadAppProfileIndex lists cluster AppProfiles once per tenant reconcile.
func loadAppProfileIndex(ctx context.Context, c client.Client) (map[string]*gentianov1alpha1.ComponentProfile, error) {
	list := &gentianov1alpha1.ComponentProfileList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list AppProfiles: %w", err)
	}
	index := make(map[string]*gentianov1alpha1.ComponentProfile, len(list.Items))
	for i := range list.Items {
		index[list.Items[i].Name] = &list.Items[i]
	}
	return index, nil
}

func appProfileFromIndex(index map[string]*gentianov1alpha1.ComponentProfile, name string) (*gentianov1alpha1.ComponentProfile, bool) {
	if index == nil || name == "" {
		return nil, false
	}
	profile, ok := index[name]
	return profile, ok
}
