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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestTenantValidatorHandleNilDecoder_DeniesMissingAppProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	validator := &TenantValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
	}

	tenant := gentianov1alpha1.Tenant{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "gentianos.io/v1alpha1",
			Kind:       "Tenant",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "gtn-demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "missing-profile-app"}},
		},
	}

	raw, err := json.Marshal(tenant)
	if err != nil {
		t.Fatalf("marshal tenant: %v", err)
	}

	resp := validator.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	})

	if resp.Allowed {
		t.Fatalf("expected request to be denied")
	}

	if resp.Result == nil {
		t.Fatalf("expected denial result message")
	}

	if !strings.Contains(resp.Result.Message, "AppProfile \"missing-profile-app\" not found") {
		t.Fatalf("unexpected denial message: %q", resp.Result.Message)
	}
}

func TestTenantValidatorHandle_NilReceiverReturnsInternalError(t *testing.T) {
	var validator *TenantValidator

	resp := validator.Handle(context.Background(), admission.Request{})

	if resp.Allowed {
		t.Fatalf("expected request to be rejected")
	}
	if resp.Result == nil || resp.Result.Code != 500 {
		t.Fatalf("expected HTTP 500 response, got: %#v", resp.Result)
	}
	if !strings.Contains(resp.Result.Message, "tenant validator is nil") {
		t.Fatalf("unexpected message: %q", resp.Result.Message)
	}
}

func TestTenantValidatorHandle_NilClientReturnsInternalError(t *testing.T) {
	tenant := gentianov1alpha1.Tenant{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "gentianos.io/v1alpha1",
			Kind:       "Tenant",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "gtn-demo"},
	}
	raw, err := json.Marshal(tenant)
	if err != nil {
		t.Fatalf("marshal tenant: %v", err)
	}

	validator := &TenantValidator{}
	resp := validator.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	})

	if resp.Allowed {
		t.Fatalf("expected request to be rejected")
	}
	if resp.Result == nil || resp.Result.Code != 500 {
		t.Fatalf("expected HTTP 500 response, got: %#v", resp.Result)
	}
	if !strings.Contains(resp.Result.Message, "client is not initialized") {
		t.Fatalf("unexpected message: %q", resp.Result.Message)
	}
}
