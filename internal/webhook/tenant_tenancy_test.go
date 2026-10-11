/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package webhook

import (
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The two tenancy modes, at admission. The handover is proven throughout, so
// nothing but the mode can refuse.
func tenancyValidator(t *testing.T, mode string) *TenantValidator {
	v := gateValidator(t, provenRecord())
	v.TenancyMode, v.KernelRealm = mode, "kernel"
	return v
}

// single: the platform tenant and exactly one user tenant, named user. A
// second user tenant is refused with a message naming the mode. The platform
// tenant is not counted.
func TestSingleTenancyAdmitsOneUserTenant(t *testing.T) {
	v := tenancyValidator(t, "single")
	if resp := handle(t, v, platformTenant(), admissionv1.Create); !resp.Allowed {
		t.Fatalf("the platform tenant was refused: %s", resp.Result.Message)
	}
	if resp := handle(t, v, bareTenant("user", nil), admissionv1.Create); !resp.Allowed {
		t.Fatalf("the one user tenant was refused: %s", resp.Result.Message)
	}
	for _, op := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		resp := handle(t, v, bareTenant("acme", nil), op)
		if resp.Allowed {
			t.Fatalf("%s: a second user tenant was admitted under single", op)
		}
		for _, want := range []string{"acme", "tenancy mode is single", `"user"`, "tenancyMode: multi"} {
			if !strings.Contains(resp.Result.Message, want) {
				t.Errorf("%s: the refusal does not say %q: %s", op, want, resp.Result.Message)
			}
		}
	}
}

// multi: the platform tenant plus any number of user tenants, one named
// user among them.
func TestMultiTenancyAdmitsAnyNumberOfTenants(t *testing.T) {
	v := tenancyValidator(t, "multi")
	for _, tenant := range []gentianov1alpha1.Tenant{platformTenant(), bareTenant("acme", nil), bareTenant("beta", nil), bareTenant("user", nil)} {
		if resp := handle(t, v, tenant, admissionv1.Create); !resp.Allowed {
			t.Fatalf("%s was refused under multi: %s", tenant.Name, resp.Result.Message)
		}
	}
}

// The user tenant of a single-tenancy cluster is held back by the handover
// gate like any tenant: it is committed with the cluster and admitted only
// once the platform administrator has signed in. No override is involved.
func TestTheUserTenantWaitsForTheHandover(t *testing.T) {
	v := gateValidator(t)
	v.TenancyMode, v.KernelRealm = "single", "kernel"
	if resp := handle(t, v, bareTenant("user", nil), admissionv1.Create); resp.Allowed {
		t.Fatal("the user tenant was admitted before the handover was proven")
	}
	if resp := handle(t, v, platformTenant(), admissionv1.Create); !resp.Allowed {
		t.Fatalf("the platform tenant was held back: %s", resp.Result.Message)
	}
	if resp := handle(t, tenancyValidator(t, "single"), bareTenant("user", nil), admissionv1.Create); !resp.Allowed {
		t.Fatalf("the user tenant was refused after the handover: %s", resp.Result.Message)
	}
}
