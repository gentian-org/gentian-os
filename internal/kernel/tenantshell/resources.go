/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package tenantshell mirrors, in Go, the quota the tenant-default
// Composition gives a tenant namespace.
package tenantshell

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResourceListFromQuotas maps a tenant's plan quantities onto the ResourceQuota
// keys that enforce them.
//
// It is NOT what creates the quota. The tenant-default Composition composes the
// real ResourceQuota through provider-kubernetes, and this mirrors that
// template — the mapping exists twice, in Go and in Go templating, and the
// Composition is the one the cluster obeys.
//
// Two readers depend on the mirror holding. The downgrade guard in
// internal/resourceplan compares a candidate plan against these keys, so a
// mapping that drifts from the Composition would pass a plan against
// requests.cpu and see it enforced against limits.cpu. And
// xtenant_shell_simulator_test.go stands in for the Composition, so a test
// suite built on a stale mirror agrees with itself and with nothing real.
//
// That is not hypothetical: requests.cpu and requests.memory were added here
// and not to the Composition, and a tenant moved onto a plan received its
// limits and none of its reserved capacity. Adding them to the Composition was
// not enough either — the quantities still have to survive the Tenant -> XTenant
// projection in internal/controller and the XRD's structural schema, and they
// survived neither, so the same tenants kept getting the same half a plan.
//
// Change one, change all four: this mirror, the Composition, xtenantQuotas in
// internal/controller, and crossplane/xrds/tenant.yaml. Two tests now fail when
// any of them drifts — composition_agreement_test.go here, and
// xtenant_quotas_agreement_test.go for the two hops between Tenant and XTenant.
func ResourceListFromQuotas(q *gentianov1alpha1.TenantQuotas) corev1.ResourceList {
	if q == nil {
		return nil
	}
	rl := corev1.ResourceList{}
	if q.Storage != nil {
		rl[corev1.ResourceRequestsStorage] = *q.Storage
	}
	if q.CPU != nil {
		rl[corev1.ResourceLimitsCPU] = *q.CPU
	}
	if q.Memory != nil {
		rl[corev1.ResourceLimitsMemory] = *q.Memory
	}
	if q.RequestsCPU != nil {
		rl[corev1.ResourceRequestsCPU] = *q.RequestsCPU
	}
	if q.RequestsMemory != nil {
		rl[corev1.ResourceRequestsMemory] = *q.RequestsMemory
	}
	if q.MaxPods > 0 {
		rl[corev1.ResourcePods] = resource.MustParse(fmt.Sprintf("%d", q.MaxPods))
	}
	return rl
}
