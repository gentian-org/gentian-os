/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package tenancy holds rules about how many tenants a cluster may carry.
package tenancy

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// EnforceSingle rejects a Tenant that a TENANCY_MODE=single cluster may not carry.
//
// One rule, two enforcement points. The admission webhook refuses the Tenant
// before it is stored; the reconciler refuses to provision one that is already
// there — from a cluster whose mode changed after the fact, or written while the
// webhook was unavailable. Both are wanted, and both must answer identically:
// a webhook that admits what the reconciler then refuses produces a Tenant that
// exists and never progresses, with the reason on a status condition nobody is
// watching.
//
// This lived twice, as TenantReconciler.validateTenancyConstraints and
// TenantValidator.validateTenancy — byte-identical but for the receiver and one
// error wrap. Nothing kept them in step except that no one had edited either.
func EnforceSingle(ctx context.Context, c client.Reader, tenancyMode string, tenant *gentianov1alpha1.Tenant) error {
	if gentianov1alpha1.NormalizeTenancyMode(tenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	if tenant.Name != gentianov1alpha1.SingleTenantName {
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only Tenant %q (got %q)",
			gentianov1alpha1.SingleTenantName, tenant.Name,
		)
	}

	var others gentianov1alpha1.TenantList
	if err := c.List(ctx, &others); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for i := range others.Items {
		other := &others.Items[i]
		// A Tenant already being deleted does not occupy the slot: it is the
		// one being replaced, and refusing its successor would make a
		// single-tenant cluster impossible to re-provision.
		if other.Name == tenant.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only one Tenant CR (found %q and %q)",
			tenant.Name, other.Name,
		)
	}
	return nil
}
