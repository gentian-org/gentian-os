/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The platform is a tenant whose realm is the kernel realm (AD-10). It exists
// before handover -- the handover is proven through its desktop -- and it is
// never deleted, because its realm is the one every administrator signs in
// through.

func platformTenant() gentianov1alpha1.Tenant {
	t := bareTenant("platform", nil)
	t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	return t
}

func handleDelete(t *testing.T, v *TenantValidator, tenant gentianov1alpha1.Tenant) admission.Response {
	t.Helper()
	raw, err := json.Marshal(tenant)
	if err != nil {
		t.Fatalf("marshal tenant: %v", err)
	}
	return v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Delete,
			OldObject: runtime.RawExtension{Raw: raw},
		},
	})
}

func TestThePlatformTenantIsAdmittedBeforeHandover(t *testing.T) {
	v := gateValidator(t)
	v.KernelRealm = "kernel"
	if resp := handle(t, v, platformTenant(), admissionv1.Create); !resp.Allowed {
		t.Fatalf("the platform tenant must be admitted before handover; denied with: %s", resp.Result.Message)
	}
	// The gate still holds for everyone else.
	if resp := handle(t, v, bareTenant("acme", nil), admissionv1.Create); resp.Allowed {
		t.Fatal("an ordinary tenant must still wait for handover")
	}
}

func TestATenantAdoptingTheKernelRealmCannotBeDeleted(t *testing.T) {
	v := gateValidator(t)
	v.KernelRealm = "kernel"
	resp := handleDelete(t, v, platformTenant())
	if resp.Allowed {
		t.Fatal("deleting the tenant that adopts the kernel realm must be refused")
	}
	if !strings.Contains(resp.Result.Message, "kernel") {
		t.Errorf("the refusal should name the realm; got: %s", resp.Result.Message)
	}
	if resp := handleDelete(t, v, bareTenant("acme", nil)); !resp.Allowed {
		t.Fatalf("an ordinary tenant is deletable; denied with: %s", resp.Result.Message)
	}
}

func TestWithoutAKernelRealmNothingIsSpecial(t *testing.T) {
	v := gateValidator(t)
	if resp := handleDelete(t, v, platformTenant()); !resp.Allowed {
		t.Fatalf("no kernel realm configured, so no tenant adopts it; denied with: %s", resp.Result.Message)
	}
}
