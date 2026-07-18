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
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestEnsureMacWaivers_annotatesApprovedWaivers(t *testing.T) {
	t.Parallel()

	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
				{Profile: "catalogue-test-app", Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
			},
		},
	}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue-test-app"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Security: &gentianov1alpha1.SecuritySpec{
				MacWaivers: []gentianov1alpha1.MacWaiverRequest{
					{Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
					{Policy: "other-policy", Scope: "other-scope"},
				},
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(psp, profile, tenant).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}

	raw := tenant.Annotations[approvedMacWaiversAnnotation]
	if raw == "" {
		t.Fatal("expected approved waivers annotation")
	}
	var approved map[string][]gentianov1alpha1.MacWaiverRequest
	if err := json.Unmarshal([]byte(raw), &approved); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if len(approved["catalogue-test-app"]) != 1 {
		t.Fatalf("approved = %#v", approved)
	}

	found := false
	for _, cond := range tenant.Status.Conditions {
		if cond.Type == conditionMacWaiversReady && cond.Reason == "WaiverNotApproved" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected MacWaiversReady=False for unapproved waiver request")
	}
}
