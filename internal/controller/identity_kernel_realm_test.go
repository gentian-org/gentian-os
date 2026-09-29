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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// A tenant that adopts the kernel realm provisions only what is its own inside
// that realm. The realm Job would recreate or reconfigure the realm every
// administrator signs in through; the admin Job would mint a second
// administrator; the broker Job would broker the kernel realm to itself. None
// of them is emitted, and the groups Job -- the tenant's entitlement groups,
// which are the tenant's -- is.
func TestATenantAdoptingTheKernelRealmProvisionsOnlyItsOwnGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).Build(),
		KernelRealm:  "kernel",
		KernelDomain: "platform.example.test",
	}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "platform"
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}

	jobs, err := r.buildIdentityProvisioningJobs(context.Background(), tenant, keycloakRealmName(tenant))
	if err != nil {
		t.Fatalf("buildIdentityProvisioningJobs: %v", err)
	}
	var names []string
	for _, j := range jobs {
		names = append(names, j.Name)
	}
	if len(jobs) != 1 || jobs[0].Name != gentianGroupsJobName("platform") {
		t.Fatalf("jobs = %v, want only %s", names, gentianGroupsJobName("platform"))
	}
	if jobs[0].Namespace != identityNamespace {
		t.Fatalf("groups Job namespace = %q, want the authentication namespace %q", jobs[0].Namespace, identityNamespace)
	}

	// And an ordinary tenant still gets its realm and administrator.
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "acme"
	jobs, err = r.buildIdentityProvisioningJobs(context.Background(), plain, keycloakRealmName(plain))
	if err != nil {
		t.Fatalf("buildIdentityProvisioningJobs(acme): %v", err)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.Name] = true
	}
	for _, want := range []string{realmJobName("acme"), gentianGroupsJobName("acme"), adminJobName("acme")} {
		if !seen[want] {
			t.Errorf("ordinary tenant lacks Job %s (got %v)", want, seen)
		}
	}
}
