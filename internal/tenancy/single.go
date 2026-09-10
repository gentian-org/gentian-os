/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
