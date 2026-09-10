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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/handover"
)

const gateNamespace = "gentian-system"

func gateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentianos scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func provenRecord() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: handover.ConfigMapName, Namespace: gateNamespace},
		Data:       map[string]string{handover.KeyWritePathProven: "true"},
	}
}

func gateValidator(t *testing.T, objs ...client.Object) *TenantValidator {
	t.Helper()
	return &TenantValidator{
		Client:            fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(objs...).Build(),
		GateOnHandover:    true,
		HandoverNamespace: gateNamespace,
	}
}

// A tenant with no apps, so nothing but the gate can deny it.
func bareTenant(name string, annotations map[string]string) gentianov1alpha1.Tenant {
	return gentianov1alpha1.Tenant{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gentianos.io/v1alpha1", Kind: "Tenant"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       gentianov1alpha1.TenantSpec{DisplayName: name},
	}
}

func handle(t *testing.T, v *TenantValidator, tenant gentianov1alpha1.Tenant, op admissionv1.Operation) admission.Response {
	t.Helper()
	raw, err := json.Marshal(tenant)
	if err != nil {
		t.Fatalf("marshal tenant: %v", err)
	}
	return v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: op,
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
}

// The gate's whole purpose: no proof, no new tenant.
func TestHandoverGate_DeniesCreateBeforeProof(t *testing.T) {
	resp := handle(t, gateValidator(t), bareTenant("acme", nil), admissionv1.Create)
	if resp.Allowed {
		t.Fatal("expected the tenant to be refused while the write path is unproven")
	}
	msg := resp.Result.Message
	// The message has to carry all three things an operator needs: which tenant,
	// what to do about it, and the way out. A denial missing any of them gets
	// answered by disabling the webhook, which takes the AppProfile and tenancy
	// checks with it.
	for _, want := range []string{"acme", "sign in", HandoverOverrideAnnotation} {
		if !strings.Contains(msg, want) {
			t.Errorf("denial message should mention %q; got: %s", want, msg)
		}
	}
}

func TestHandoverGate_AllowsCreateAfterProof(t *testing.T) {
	resp := handle(t, gateValidator(t, provenRecord()), bareTenant("acme", nil), admissionv1.Create)
	if !resp.Allowed {
		t.Fatalf("expected admission once proven; denied with: %s", resp.Result.Message)
	}
}

// Updates are never gated: a cluster mid-handover must still be able to fix the
// tenant that is misconfigured, or the precaution becomes a trap.
func TestHandoverGate_AllowsUpdateBeforeProof(t *testing.T) {
	resp := handle(t, gateValidator(t), bareTenant("acme", nil), admissionv1.Update)
	if !resp.Allowed {
		t.Fatalf("expected an update to be admitted regardless of proof; denied with: %s", resp.Result.Message)
	}
}

func TestHandoverGate_OverrideAdmitsWithReason(t *testing.T) {
	tenant := bareTenant("acme", map[string]string{HandoverOverrideAnnotation: "migrating from the old cluster"})
	resp := handle(t, gateValidator(t), tenant, admissionv1.Create)
	if !resp.Allowed {
		t.Fatalf("expected the annotated tenant to be admitted; denied with: %s", resp.Result.Message)
	}
}

// An empty annotation is not an override. Otherwise `kubectl annotate ...=""`
// silently disables the gate while reading as though a reason were recorded.
func TestHandoverGate_EmptyOverrideIsNotAnOverride(t *testing.T) {
	tenant := bareTenant("acme", map[string]string{HandoverOverrideAnnotation: ""})
	if handle(t, gateValidator(t), tenant, admissionv1.Create).Allowed {
		t.Fatal("an override with no reason should not admit")
	}
}

func TestHandoverGate_DisabledAdmits(t *testing.T) {
	v := gateValidator(t)
	v.GateOnHandover = false
	if !handle(t, v, bareTenant("acme", nil), admissionv1.Create).Allowed {
		t.Fatal("expected admission when the gate is switched off")
	}
}

// An unset namespace disables the gate rather than refusing everything: a
// misconfigured operator should not be able to block every tenant on the
// cluster by omission.
func TestHandoverGate_NoNamespaceAdmits(t *testing.T) {
	v := gateValidator(t)
	v.HandoverNamespace = ""
	if !handle(t, v, bareTenant("acme", nil), admissionv1.Create).Allowed {
		t.Fatal("expected admission when no handover namespace is configured")
	}
}
