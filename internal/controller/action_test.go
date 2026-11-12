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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const stepBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: ocr
  labels:
    gentianos.io/profile-name: ocr
spec:
  classes: [action]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    images:
    - repository: registry.example.invalid/steps/ocr
      digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
  action:
    command: ["/ocr"]
`

// An action is installed and nothing is deployed for it: it is Ready once its
// profile is the one the install names, and says which image a run starts.
// An instance of another class rolls out nothing either.
func TestAnActionIsInstalledWithoutAWorkload(t *testing.T) {
	h := startDigestHarness(t, materialised(t, stepBundle), "")
	h.comp.Spec.Class = gentianov1alpha1.ComponentClassAction
	if err := h.c.Update(context.Background(), h.comp); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "True" || ready.Reason != "Ready" {
		t.Fatalf("condition = %+v, want Ready", ready)
	}
	for _, said := range []string{"registry.example.invalid/steps/ocr@sha256:0000", "nothing is deployed", "dispatcher"} {
		if !strings.Contains(ready.Message, said) {
			t.Errorf("message %q does not say %q", ready.Message, said)
		}
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatalf("an action was rolled out: release %q, network policy %v", h.releasedVersion(), h.networkPolicyWritten())
	}

	h = startDigestHarness(t, materialised(t, stepBundle), "")
	ready = componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != reasonClassMismatch {
		t.Fatalf("an action installed as an app: condition = %+v, want %s", ready, reasonClassMismatch)
	}
	if h.releasedVersion() != "" {
		t.Fatalf("an action rolled out as an app: release %q", h.releasedVersion())
	}
}

// An install takes its class from its profile, for an action as for an
// adapter.
func TestAnActionInstallRunsAsAnAction(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "ocr"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("ocr", false, gentianov1alpha1.ComponentClassAction),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	comp := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, types.NamespacedName{Name: "ocr", Namespace: tenantNamespaceName(tenant)}, comp); err != nil {
		t.Fatal(err)
	}
	if comp.Spec.Class != gentianov1alpha1.ComponentClassAction || comp.Annotations[componentClassAssumedAnnotation] != "" {
		t.Fatalf("class %q, annotations %v", comp.Spec.Class, comp.Annotations)
	}
}
