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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func (r *TenantReconciler) collectTenantIngressIntents(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]ingressIntent, error) {
	return collectTenantIngressIntents(ctx, r.Client, tenant)
}

func attachBackendTrafficPolicyTarget(spec map[string]interface{}, routeName string) {
	refs, ok := spec["targetRefs"].([]interface{})
	if !ok || len(refs) == 0 {
		return
	}
	ref, ok := refs[0].(map[string]interface{})
	if !ok {
		return
	}
	ref["name"] = routeName
}
