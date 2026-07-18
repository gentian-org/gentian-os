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
