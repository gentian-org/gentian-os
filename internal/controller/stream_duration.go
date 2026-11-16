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

// sessionStreamDuration is how long one stream through the edge may stay
// open: as long as the longest sign-in any realm on this cluster allows --
// the kernel realm's and every tenant's (Tenant.SessionMaxHours) -- and no
// longer. A WebSocket is one stream, so a connection opened while signed in
// ends no later than the longest session could have.
func sessionStreamDuration(ctx context.Context, c client.Reader) (string, error) {
	hours := gentianov1alpha1.DefaultSessionMaxHours
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return "", fmt.Errorf("list tenants for the longest session: %w", err)
	}
	for i := range tenants.Items {
		if tenants.Items[i].DeletionTimestamp != nil {
			continue
		}
		if h := tenants.Items[i].SessionMaxHours(); h > hours {
			hours = h
		}
	}
	return fmt.Sprintf("%dh", hours), nil
}

// withMaxStreamDuration is spec with timeout.http.maxStreamDuration set to
// d, its other timeouts kept. spec is not changed: the maps on the way are
// copied.
func withMaxStreamDuration(spec map[string]interface{}, d string) map[string]interface{} {
	out := cloneMap(spec)
	timeout := map[string]interface{}{}
	if t, ok := spec["timeout"].(map[string]interface{}); ok {
		timeout = cloneMap(t)
	}
	http := map[string]interface{}{}
	if h, ok := timeout["http"].(map[string]interface{}); ok {
		http = cloneMap(h)
	}
	http["maxStreamDuration"] = d
	timeout["http"] = http
	out["timeout"] = timeout
	return out
}
