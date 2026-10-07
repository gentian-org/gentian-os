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
	"errors"
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ErrSingleTenancy is the refusal of a tenant a single-tenancy cluster may
// not carry. Wrapped by EnforceSingle, so a caller can tell this refusal from
// a failure to ask.
var ErrSingleTenancy = errors.New("this cluster's tenancy mode is single")

// IsPlatformTenant reports whether a tenant is the platform's own: the one
// whose identity lives in the kernel realm. By the field and not by its name,
// which is what the operator, the webhook and the director all go by.
func IsPlatformTenant(tenant *gentianov1alpha1.Tenant, kernelRealm string) bool {
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	return tenant != nil && tenant.Spec.Isolation != nil && tenant.Spec.Isolation.KeycloakRealm == kernelRealm
}

// SingleRefusal is what is wrong with a tenant of this name on a
// single-tenancy cluster, or nothing. The platform tenant is not asked about:
// it is in every cluster and is never counted.
//
// A single-tenancy cluster carries exactly one user tenant, and its name is
// fixed. Fixing the name is what makes "exactly one" a property of the name
// rather than of a count somebody has to take: there cannot be two objects of
// one name, so nothing has to list the others to refuse the second, and the
// webhook, the reconciler and the director cannot come to different answers
// from different views of the cluster.
func SingleRefusal(name string) error {
	if name == gentianov1alpha1.SingleUserTenantName {
		return nil
	}
	return fmt.Errorf(
		"%w: it carries the platform tenant and exactly one user tenant, named %q, "+
			"so tenant %q is refused. A cluster for more than one user tenant sets tenancyMode: multi",
		ErrSingleTenancy, gentianov1alpha1.SingleUserTenantName, name,
	)
}

// EnforceSingle rejects a Tenant that a tenancyMode=single cluster may not
// carry: any user tenant but the one named "user". Under multi it refuses
// nothing, and under either mode the platform tenant is not counted.
//
// One rule, two enforcement points in the operator and a third in the
// director. The admission webhook refuses the Tenant before it is stored; the
// reconciler refuses to provision one that is already there -- from a cluster
// whose mode changed from multi after the fact, or written while the webhook
// was unavailable -- and says so on the tenant's status without touching
// anything the tenant already has. All must answer identically: a webhook
// that admits what the reconciler then refuses produces a Tenant that exists
// and never progresses.
func EnforceSingle(tenancyMode, kernelRealm string, tenant *gentianov1alpha1.Tenant) error {
	if gentianov1alpha1.NormalizeTenancyMode(tenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	if IsPlatformTenant(tenant, kernelRealm) {
		return nil
	}
	return SingleRefusal(tenant.Name)
}
