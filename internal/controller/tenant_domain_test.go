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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A TenantDomain moves its tenant to the domain it names; without one the
// tenant is where tenancy mode puts it; one on the kernel domain is refused
// and says so, leaving the tenant at its default.
func TestATenantDomainMovesItsTenant(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	binding := &gentianov1alpha1.TenantDomain{Spec: gentianov1alpha1.TenantDomainSpec{Domain: "Acme.Example"}}
	binding.Name = "acme"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(binding).Build()
	r := &TenantReconciler{Client: c, KernelDomain: "k.example", TenancyMode: "multi"}

	tenant := acmeTenantFixture()
	r.resolveTenantDomain(ctx, tenant)
	if got := tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode); got != "acme.example" {
		t.Fatalf("bound: effective domain = %q", got)
	}
	if !meta.IsStatusConditionTrue(tenant.Status.Conditions, conditionDomainBound) {
		t.Fatal("bound: DomainBound is not True")
	}

	binding.Spec.Domain = "acme.k.example"
	if err := c.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	r.resolveTenantDomain(ctx, tenant)
	if got := tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode); got != "acme.k.example" || tenant.Status.Domain != "" {
		t.Fatalf("under the kernel domain: status.domain = %q", tenant.Status.Domain)
	}
	if cond := meta.FindStatusCondition(tenant.Status.Conditions, conditionDomainBound); cond == nil || cond.Reason != "UnderKernelDomain" {
		t.Fatalf("under the kernel domain: condition = %+v", cond)
	}

	if err := c.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	r.resolveTenantDomain(ctx, tenant)
	if tenant.Status.Domain != "" || meta.FindStatusCondition(tenant.Status.Conditions, conditionDomainBound) != nil {
		t.Fatalf("unbound: status.domain = %q, conditions = %v", tenant.Status.Domain, tenant.Status.Conditions)
	}
}

// Two TenantDomains naming one domain: the one bound first keeps it, and the
// other tenant stays where it was and says whose the domain is.
func TestADomainTwoTenantsClaimStaysWithTheFirst(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	first := &gentianov1alpha1.TenantDomain{Spec: gentianov1alpha1.TenantDomainSpec{Domain: "shared.example"}}
	first.Name = "acme"
	first.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	second := &gentianov1alpha1.TenantDomain{Spec: gentianov1alpha1.TenantDomainSpec{Domain: "shared.example"}}
	second.Name = "beta"
	second.CreationTimestamp = metav1.NewTime(time.Unix(2000, 0))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(first, second).Build()
	r := &TenantReconciler{Client: c, KernelDomain: "k.example", TenancyMode: "multi"}

	acme := acmeTenantFixture()
	r.resolveTenantDomain(ctx, acme)
	if acme.Status.Domain != "shared.example" {
		t.Fatalf("the first claim lost: %q", acme.Status.Domain)
	}
	beta := &gentianov1alpha1.Tenant{}
	beta.Name = "beta"
	r.resolveTenantDomain(ctx, beta)
	if beta.Status.Domain != "" {
		t.Fatalf("the second claim won: %q", beta.Status.Domain)
	}
	if cond := meta.FindStatusCondition(beta.Status.Conditions, conditionDomainBound); cond == nil || cond.Reason != "DomainTaken" {
		t.Fatalf("condition = %+v", cond)
	}
}
