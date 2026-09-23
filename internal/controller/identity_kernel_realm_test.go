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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The platform is a tenant whose realm is the kernel realm (AD-10), and any
// tenant can be pointed at it by hand. Deleting such a tenant must not disable
// the realm every administrator signs in through: that locks all of them out
// of the cluster at once, and there is nothing to undo, because a tenant that
// adopts a realm never created it.
//
// The reconciler here has no client on purpose. Reaching one means the guard
// did not return, which is the failure this pins.
func TestDeletingATenantNeverTouchesTheKernelRealm(t *testing.T) {
	for _, policy := range []gentianov1alpha1.DeletionPolicy{
		gentianov1alpha1.DeletionPolicyDelete,
		gentianov1alpha1.DeletionPolicyRetain,
	} {
		r := &TenantReconciler{KernelRealm: "kernel"}
		tenant := &gentianov1alpha1.Tenant{}
		tenant.Name = "platform"
		tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
		tenant.Spec.DeletionPolicy = policy

		if err := r.deleteIdentity(context.Background(), tenant); err != nil {
			t.Fatalf("deletionPolicy %s: deleteIdentity = %v, want nil and no work", policy, err)
		}
	}
}
