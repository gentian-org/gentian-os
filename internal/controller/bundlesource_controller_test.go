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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// A source is given what a destination is given -- a requirement the
// custodian fills at a kernel path derived from its name, a probe, and a
// Secret beside the object store -- each owned by it, and it says it waits
// for its keys until they are there.
func TestASourceIsGivenItsCredentialPathAsADestinationIs(t *testing.T) {
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	src := &gentianov1alpha1.BundleSource{
		ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "u-old"},
		Spec:       gentianov1alpha1.BundleSourceSpec{Endpoint: "https://s3.example", Bucket: "bundles"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(src).WithStatusSubresource(src).Build()
	r := &BundleSourceReconciler{Client: c, Scheme: s}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "old"}}); err != nil {
		t.Fatal(err)
	}

	var req gentianov1alpha1.CredentialRequirement
	if err := c.Get(ctx, types.NamespacedName{Name: "bundle-source-old"}, &req); err != nil {
		t.Fatal(err)
	}
	if req.Spec.VaultPath != "gentian-os/kernel/backup/sources/old" || req.Spec.Scope != "cluster" ||
		len(req.OwnerReferences) != 1 || req.OwnerReferences[0].Name != "old" {
		t.Errorf("requirement %+v", req)
	}
	for _, es := range []types.NamespacedName{
		{Name: credentialProbePrefix + "bundle-source-old", Namespace: meta.OperatorNamespace},
		{Name: backup.SourceCredentialName("old"), Namespace: s3Namespace},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(externalSecretGVK)
		if err := c.Get(ctx, es, obj); err != nil {
			t.Fatalf("%s: %v", es, err)
		}
		if refs := obj.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "old" {
			t.Errorf("%s is not the source's: %+v", es, refs)
		}
	}

	var got gentianov1alpha1.BundleSource
	if err := c.Get(ctx, types.NamespacedName{Name: "old"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.CredentialSatisfied || len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Reason != "CredentialUnsatisfied" {
		t.Errorf("status %+v", got.Status)
	}

	if reqs := sourceForProbe(ctx, &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": credentialProbePrefix + "bundle-source-old"}}}); len(reqs) != 1 || reqs[0].Name != "old" {
		t.Errorf("the probe wakes %v", reqs)
	}
}
