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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app that calls its file home as the person is told the home's scope in
// its chart's values while it is granted the contract, and nothing once the
// grant is withdrawn. A profile that names no key is told nothing.
func TestAGrantedHomesScopeIsAValueOfTheChart(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	ns := "tenant-acme"

	editor := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "editor"}}
	editor.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "file-store", AsPerson: true}}
	editor.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
			"file-store": {EndpointKey: "home.url", HomeScopeKey: "home.scope"},
		},
	}
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "editor", Namespace: ns}}
	binding := func(consumer string, granted metav1.ConditionStatus) *gentianov1alpha1.IntegrationBinding {
		ib := &gentianov1alpha1.IntegrationBinding{ObjectMeta: metav1.ObjectMeta{Name: consumer + "-file-store", Namespace: ns}}
		ib.Spec.Contract = "file-store"
		ib.Spec.Consumer.App = consumer
		ib.Spec.Provider.App = "nextcloud"
		ib.Status.Conditions = []metav1.Condition{{Type: conditionBindingGranted, Status: granted, Reason: "x"}}
		return ib
	}
	values := func(objs ...*gentianov1alpha1.IntegrationBinding) map[string]interface{} {
		t.Helper()
		b := fake.NewClientBuilder().WithScheme(scheme)
		for _, o := range objs {
			b = b.WithObjects(o)
		}
		out, err := homeScopeValues(context.Background(), b.Build(), comp, editor)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := values(binding("editor", metav1.ConditionTrue), binding("other", metav1.ConditionTrue))
	home, _ := got["home"].(map[string]interface{})
	if home["scope"] != "home-nextcloud" || len(got) != 1 || len(home) != 1 {
		t.Fatalf("granted: values = %v", got)
	}
	if got := values(binding("editor", metav1.ConditionFalse)); len(got) != 0 {
		t.Fatalf("withdrawn: values = %v", got)
	}

	silent := editor.DeepCopy()
	silent.Spec.Package.ValueMapping.Integrations["file-store"] = gentianov1alpha1.IntegrationValueMapping{EndpointKey: "home.url"}
	out, err := homeScopeValues(context.Background(),
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(binding("editor", metav1.ConditionTrue)).Build(), comp, silent)
	if err != nil || len(out) != 0 {
		t.Fatalf("no key named: values = %v, %v", out, err)
	}
}
