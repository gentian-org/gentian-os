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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
)

func TestPlatformSecurityReconciler_SyncsConfigMap(t *testing.T) {
	t.Parallel()

	allowed := []gentianov1alpha1.AllowedMacWaiver{
		{Profile: "catalogue-test-app", Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
	}
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: allowed,
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(psp).WithStatusSubresource(psp).Build()

	r := &PlatformSecurityPolicyReconciler{Client: c}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: gentianov1alpha1.PlatformSecurityPolicyName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      gentianov1alpha1.PlatformSecurityConfigMapName,
		Namespace: layout.Namespace(layout.Control),
	}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}

	got, err := security.ParseAllowedMacWaiversFromConfigMap(cm.Data[gentianov1alpha1.PlatformSecurityConfigMapKey])
	if err != nil {
		t.Fatalf("parse ConfigMap: %v", err)
	}
	if len(got) != 1 || got[0].Profile != "catalogue-test-app" {
		t.Fatalf("allowed waivers = %#v", got)
	}
}

func TestPlatformSecurityReconciler_UpdatesExistingConfigMap(t *testing.T) {
	t.Parallel()

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gentianov1alpha1.PlatformSecurityConfigMapName,
			Namespace: layout.Namespace(layout.Control),
		},
		Data: map[string]string{
			gentianov1alpha1.PlatformSecurityConfigMapKey: `[{"profile":"old","policy":"p","scope":"s"}]`,
		},
	}
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
				{Profile: "new-app", Policy: "gentian-require-non-root", Scope: "sidecar"},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing, psp).WithStatusSubresource(psp).Build()

	r := &PlatformSecurityPolicyReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: gentianov1alpha1.PlatformSecurityPolicyName},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      gentianov1alpha1.PlatformSecurityConfigMapName,
		Namespace: layout.Namespace(layout.Control),
	}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if !strings.Contains(cm.Data[gentianov1alpha1.PlatformSecurityConfigMapKey], "new-app") {
		t.Fatalf("ConfigMap not updated: %q", cm.Data[gentianov1alpha1.PlatformSecurityConfigMapKey])
	}
}
