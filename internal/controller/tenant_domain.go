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
	tenant.Status.Domain = domain
	r.setCondition(tenant, conditionDomainBound, metav1.ConditionTrue, "Bound", "The tenant is served at "+domain)
}
