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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A profile pinned to a new chart version reaches the Release through the
// components of that profile, and only those.
func TestAProfileChangeRunsItsComponents(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	desktopA := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "tenant-platform"}}
	desktopA.Spec.ProfileRef.Name = "desktop"
	desktopB := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "tenant-demo"}}
	desktopB.Spec.ProfileRef.Name = "desktop"
	other := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo"}}
	other.Spec.ProfileRef.Name = "wiki"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desktopA, desktopB, other).Build()

	got := componentsReferencingProfile(context.Background(), c, "desktop")
	if len(got) != 2 {
		t.Fatalf("requests = %v, want the two desktops", got)
	}
	for _, req := range got {
		if req.Name != "desktop" {
			t.Fatalf("request %v is not a desktop", req)
		}
	}
	if got := componentsReferencingProfile(context.Background(), c, "nothing"); len(got) != 0 {
		t.Fatalf("a profile nothing references runs %v", got)
	}
}
