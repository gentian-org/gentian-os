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
