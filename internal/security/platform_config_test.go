/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestSyncPlatformSecurityConfigMap_roundTrip(t *testing.T) {
	t.Parallel()
	allowed := []gentianov1alpha1.AllowedMacWaiver{
		{Profile: "catalogue-test-app", Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := SyncPlatformSecurityConfigMap(context.Background(), c, allowed); err != nil {
		t.Fatalf("SyncPlatformSecurityConfigMap: %v", err)
	}

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name:      gentianov1alpha1.PlatformSecurityConfigMapName,
		Namespace: operatorNamespace,
	}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	got, err := ParseAllowedMacWaiversFromConfigMap(cm.Data[gentianov1alpha1.PlatformSecurityConfigMapKey])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Scope != "sidecar-meet" {
		t.Fatalf("got = %#v", got)
	}
}
