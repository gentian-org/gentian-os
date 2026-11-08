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
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// conditionDomainBound reports a TenantDomain naming this tenant: True once
// its domain is the tenant's, False when it cannot be. Absent without one.
const conditionDomainBound = "DomainBound"

// resolveTenantDomain copies the custom domain a TenantDomain binds to this
// tenant into status.domain, which EffectiveDomain reads, and clears it when
// the binding is gone. The status is written with the rest at the end of the
// pass; this pass already uses the new value.
//
// A domain on or under the kernel domain is refused: it would put the tenant
// on hosts the kernel or another tenant answers on, and <tenant>.<kernel> is
// already where every tenant is without one.
func (r *TenantReconciler) resolveTenantDomain(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	binding := &gentianov1alpha1.TenantDomain{}
	err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, binding)
	if err != nil {
		if !errors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			// Unknown is not unbound: keep what the tenant had rather than
			// moving it off its domain over a failed read.
			log.FromContext(ctx).Error(err, "read TenantDomain", "tenant", tenant.Name)
			return
		}
		tenant.Status.Domain = ""
		meta.RemoveStatusCondition(&tenant.Status.Conditions, conditionDomainBound)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(binding.Spec.Domain))
	if kd := strings.ToLower(r.KernelDomain); kd != "" && (domain == kd || strings.HasSuffix(domain, "."+kd)) {
		tenant.Status.Domain = ""
		r.setCondition(tenant, conditionDomainBound, metav1.ConditionFalse, "UnderKernelDomain",
			fmt.Sprintf("%s is on the kernel domain %s; the tenant stays at its default domain", domain, kd))
		return
	}
	// Two tenants on one domain would claim the same hosts. The director
	// refuses the second; a TenantDomain applied by other means is refused
	// here, and the one bound first keeps it.
	if holder := r.domainHolder(ctx, binding); holder != "" {
		tenant.Status.Domain = ""
		r.setCondition(tenant, conditionDomainBound, metav1.ConditionFalse, "DomainTaken",
			fmt.Sprintf("%s is tenant %s's; the tenant stays at its default domain", domain, holder))
		return
	}
	tenant.Status.Domain = domain
	r.setCondition(tenant, conditionDomainBound, metav1.ConditionTrue, "Bound", "The tenant is served at "+domain)
}

// domainHolder names another tenant whose TenantDomain claims the same domain
// and came first -- the older binding, and by name between two of an age --
// or "" when there is none.
func (r *TenantReconciler) domainHolder(ctx context.Context, binding *gentianov1alpha1.TenantDomain) string {
	all := &gentianov1alpha1.TenantDomainList{}
	if err := r.List(ctx, all); err != nil {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(binding.Spec.Domain))
	for i := range all.Items {
		other := &all.Items[i]
		if other.Name == binding.Name || strings.ToLower(strings.TrimSpace(other.Spec.Domain)) != domain {
			continue
		}
		earlier := other.CreationTimestamp.Before(&binding.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&binding.CreationTimestamp) && other.Name < binding.Name)
		if earlier {
			return other.Name
		}
	}
	return ""
}
