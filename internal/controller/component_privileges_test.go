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
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// componentReadyCondition is the one condition a Component carries.
func componentReadyCondition(comp *gentianov1alpha1.Component) *metav1.Condition {
	for i := range comp.Status.Conditions {
		if comp.Status.Conditions[i].Type == conditionComponentReady {
			return &comp.Status.Conditions[i]
		}
	}
	return nil
}

// A profile that needs outbound network beyond the tenant baseline, which is
// the ordinary case: an app that sends mail through a relay the platform does
// not run.
func profileRequestingEgress(name string) *gentianov1alpha1.ComponentProfile {
	p := profileFixture(name, false, gentianov1alpha1.ComponentClassApp)
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Privileges: &gentianov1alpha1.PrivilegeRequest{
			Egress: []gentianov1alpha1.EgressRequest{{
				Name:   "smtp-relay",
				Reason: "sends invitations through the customer's relay",
				Rule: networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"},
					}},
				},
			}},
		},
	}
	return p
}

// The whole of AD-5 in one assertion: a declared privilege is a request, so
// the component reports it pending and the reconciler stops. It has written
// nothing -- no network policy, no release -- because a component waiting for
// an answer should leave no half-built footprint in the namespace.
func TestAnUngrantedPrivilegeHoldsTheInstall(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	profile := profileRequestingEgress("nextcloud")
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nextcloud", Namespace: tenantNamespaceName(tenant),
			Finalizers: []string{componentFinalizer},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "nextcloud"},
			Class:      gentianov1alpha1.ComponentClassApp,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(comp)}); err != nil {
		t.Fatal(err)
	}

	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.PendingPrivileges) != 1 || got.Status.PendingPrivileges[0] != "egress/smtp-relay" {
		t.Fatalf("pendingPrivileges = %v, want the one request", got.Status.PendingPrivileges)
	}
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "PrivilegesPending" {
		t.Fatalf("condition = %+v, want PrivilegesPending", ready)
	}
	// Nothing was built while it waits.
	np := &networkingv1.NetworkPolicy{}
	err := c.Get(context.Background(), types.NamespacedName{
		Name: componentNetworkPolicyName(comp), Namespace: comp.Namespace}, np)
	if err == nil {
		t.Fatalf("a network policy was written for a component that is waiting for an approval")
	}
}

// Once somebody granted it, the component proceeds and the rule reaches the
// policy. This is what makes a grant mean something rather than be recorded.
func TestAGrantedPrivilegeReachesTheNetworkPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	profile := profileRequestingEgress("nextcloud")
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nextcloud", Namespace: tenantNamespaceName(tenant),
			Finalizers: []string{componentFinalizer},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "nextcloud"},
			Class:      gentianov1alpha1.ComponentClassApp,
			Privileges: []gentianov1alpha1.PrivilegeGrant{{
				Privilege: "egress/smtp-relay", Approver: "u-tom",
				ApprovedAt: metav1.Now(), Reason: "agreed in the security review",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(comp)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.PendingPrivileges) != 0 {
		t.Fatalf("pendingPrivileges = %v, want none", got.Status.PendingPrivileges)
	}
	if ready := componentReadyCondition(got); ready != nil && ready.Reason == "PrivilegesPending" {
		t.Fatalf("still holding on a privilege that was granted")
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: componentNetworkPolicyName(comp), Namespace: comp.Namespace}, np); err != nil {
		t.Fatalf("no network policy after the grant: %v", err)
	}
	found := false
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock != nil && to.IPBlock.CIDR == "203.0.113.0/24" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the granted rule is not in the policy: %+v", np.Spec.Egress)
	}
}

// The Tenant is where a grant comes from, because the Tenant comes from git.
// The operator copies it onto the Component, so an approval takes effect
// without anybody touching the Component.
func TestATenantsGrantsReachItsComponents(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	tenant.Spec.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{
		{Install: "desktop", Privilege: "egress/smtp-relay", Approver: "u-tom",
			ApprovedAt: metav1.Now(), Reason: "agreed in the security review"},
		// For a component this tenant does not have. Kept, and ignored.
		{Install: "nextcloud", Privilege: "egress/webhooks", Approver: "u-tom",
			ApprovedAt: metav1.Now(), Reason: "approved before it was uninstalled"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	desktop := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "desktop", Namespace: tenantNamespaceName(tenant)}, desktop); err != nil {
		t.Fatal(err)
	}
	if len(desktop.Spec.Privileges) != 1 || desktop.Spec.Privileges[0].Privilege != "egress/smtp-relay" {
		t.Fatalf("the desktop's grants = %+v", desktop.Spec.Privileges)
	}

	// An approval that arrives afterwards reaches a component that already
	// exists -- the case that matters, because a component holding on a
	// pending privilege is exactly one that was installed before the answer.
	tenant.Spec.Privileges = append(tenant.Spec.Privileges, gentianov1alpha1.TenantPrivilegeGrant{
		Install: "desktop", Privilege: "podSecurity/run-as-root", Approver: "u-sam",
		ApprovedAt: metav1.Now(), Reason: "the security officer agreed on the 20th",
	})
	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "desktop", Namespace: tenantNamespaceName(tenant)}, desktop); err != nil {
		t.Fatal(err)
	}
	if len(desktop.Spec.Privileges) != 2 {
		t.Fatalf("a later approval did not reach the component: %+v", desktop.Spec.Privileges)
	}
}

// A grant that has expired is not a grant, so the install holds again. The
// component is not broken by the expiry; it is back where it was before
// anybody approved.
func TestAnExpiredGrantHoldsTheInstallAgain(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	yesterday := metav1.NewTime(time.Now().Add(-time.Hour))
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nextcloud", Namespace: tenantNamespaceName(tenant),
			Finalizers: []string{componentFinalizer},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "nextcloud"},
			Class:      gentianov1alpha1.ComponentClassApp,
			Privileges: []gentianov1alpha1.PrivilegeGrant{{
				Privilege: "egress/smtp-relay", Approver: "u-tom",
				ApprovedAt: metav1.Now(), Reason: "agreed for one quarter only",
				ExpiresAt: &yesterday,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profileRequestingEgress("nextcloud"), comp).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(comp)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.PendingPrivileges) != 1 {
		t.Fatalf("an expired grant left pendingPrivileges = %v", got.Status.PendingPrivileges)
	}
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "PrivilegesPending" {
		t.Fatalf("condition = %+v, want PrivilegesPending", ready)
	}
}

// A profile that asks for nothing leaves the field unset rather than empty,
// so a status that nobody needs does not appear in every component's object.
func TestAProfileAskingForNoPrivilegeLeavesTheFieldUnset(t *testing.T) {
	if got := security.RequestedPrivileges(profileFixture("plain", false)); got != nil {
		t.Fatalf("requested = %v", got)
	}
}
