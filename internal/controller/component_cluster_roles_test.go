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
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// A set with one role, as a build that ships one would have it. The
// platform's own set is empty, so the gate is shown on a set of the test's.
var testClusterRoles = security.ClusterRoleSet{"read-nodes": "reads the nodes and their capacity"}

type clusterRoleCase struct {
	// Each of these is what is needed for the role to be bound; a case takes
	// one away.
	request   gentianov1alpha1.ClusterRoleRequest
	roleThere bool
	allowed   []gentianov1alpha1.AllowedClusterRole
	grants    []gentianov1alpha1.PrivilegeGrant
}

func clusterRoleEverythingInPlace() clusterRoleCase {
	return clusterRoleCase{
		request: gentianov1alpha1.ClusterRoleRequest{
			Name: "read-nodes", ServiceAccount: "dashboard", Reason: "the dashboard lists node capacity",
		},
		roleThere: true,
		allowed:   []gentianov1alpha1.AllowedClusterRole{{Profile: "dashboard", Role: "read-nodes"}},
		grants: []gentianov1alpha1.PrivilegeGrant{{
			Privilege: "clusterRoles/read-nodes", Approver: "officer",
			ApprovedAt: metav1.Now(), Reason: "reviewed for this install",
		}},
	}
}

func (c clusterRoleCase) build(t *testing.T) (*ComponentReconciler, *gentianov1alpha1.Component, *gentianov1alpha1.ComponentProfile) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "dashboard"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					ClusterRoles: []gentianov1alpha1.ClusterRoleRequest{c.request},
				},
			},
		},
	}
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "dashboard", Namespace: "tenant-demo"},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "dashboard"},
			Privileges: c.grants,
		},
	}
	objs := []client.Object{&gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec:       gentianov1alpha1.PlatformSecurityPolicySpec{AllowedClusterRoles: c.allowed},
	}}
	if c.roleThere {
		objs = append(objs, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
			Name:   security.ClusterRoleObjectName("read-nodes"),
			Labels: map[string]string{security.ClusterRoleNameLabel: "read-nodes"},
		}})
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &ComponentReconciler{Client: cl, Scheme: scheme, ClusterRoles: testClusterRoles}, comp, profile
}

func clusterRoleBindings(t *testing.T, r *ComponentReconciler) []rbacv1.ClusterRoleBinding {
	t.Helper()
	list := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(context.Background(), list); err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	return list.Items
}

func clusterRolesCondition(comp *gentianov1alpha1.Component) *metav1.Condition {
	for i := range comp.Status.Conditions {
		if comp.Status.Conditions[i].Type == conditionClusterRolesBound {
			return &comp.Status.Conditions[i]
		}
	}
	return nil
}

// With the role in the platform's set, permitted by the cluster and granted
// on the install, it is bound: that role, to that ServiceAccount in the
// component's own namespace, and nothing else.
func TestAClusterRoleIsBoundWhenAllThreeHold(t *testing.T) {
	r, comp, profile := clusterRoleEverythingInPlace().build(t)
	if err := r.ensureClusterRoles(context.Background(), comp, profile); err != nil {
		t.Fatalf("ensureClusterRoles: %v", err)
	}
	bindings := clusterRoleBindings(t, r)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want one", len(bindings))
	}
	b := bindings[0]
	if b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != "gentian-profile-role-read-nodes" {
		t.Fatalf("bound to %+v, want the platform's role", b.RoleRef)
	}
	want := rbacv1.Subject{Kind: "ServiceAccount", Name: "dashboard", Namespace: "tenant-demo"}
	if len(b.Subjects) != 1 || b.Subjects[0] != want {
		t.Fatalf("subjects = %+v, want only %+v", b.Subjects, want)
	}
	if cond := clusterRolesCondition(comp); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True", cond)
	}
}

// Each of the conditions is necessary. Take any one away and nothing is
// bound, and the Component says which one it was.
func TestAClusterRoleIsNotBoundWhenAnyConditionIsMissing(t *testing.T) {
	expired := metav1.NewTime(time.Now().Add(-time.Hour))
	cases := map[string]struct {
		change func(*clusterRoleCase)
		reason string
	}{
		"the name is not in the platform's set": {func(c *clusterRoleCase) {
			c.request.Name = "read-everything"
			c.allowed = []gentianov1alpha1.AllowedClusterRole{{Profile: "dashboard", Role: "read-everything"}}
			c.grants[0].Privilege = "clusterRoles/read-everything"
		}, security.ClusterRoleUnknown},
		"the cluster's allowlist does not permit it": {func(c *clusterRoleCase) {
			c.allowed = nil
		}, security.ClusterRoleNotAllowed},
		"the allowlist permits it for another profile": {func(c *clusterRoleCase) {
			c.allowed[0].Profile = "other"
		}, security.ClusterRoleNotAllowed},
		"it was not granted on the install": {func(c *clusterRoleCase) {
			c.grants = nil
		}, security.ClusterRoleNotGranted},
		"the grant has expired": {func(c *clusterRoleCase) {
			c.grants[0].ExpiresAt = &expired
		}, security.ClusterRoleNotGranted},
		"the grant is for another kind of privilege": {func(c *clusterRoleCase) {
			c.grants[0].Privilege = "egress/read-nodes"
		}, security.ClusterRoleNotGranted},
		"the profile states rules of its own": {func(c *clusterRoleCase) {
			c.request.Rules = []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
		}, security.ClusterRoleFreeFormRules},
		"the profile names no ServiceAccount": {func(c *clusterRoleCase) {
			c.request.ServiceAccount = ""
		}, security.ClusterRoleNoServiceAccount},
		"the profile names the namespace's default ServiceAccount": {func(c *clusterRoleCase) {
			c.request.ServiceAccount = "default"
		}, security.ClusterRoleNoServiceAccount},
		"the platform's role is not on the cluster": {func(c *clusterRoleCase) {
			c.roleThere = false
		}, "RoleNotInstalled"},
	}
	for name, tc := range cases {
		c := clusterRoleEverythingInPlace()
		tc.change(&c)
		r, comp, profile := c.build(t)
		if err := r.ensureClusterRoles(context.Background(), comp, profile); err != nil {
			t.Fatalf("%s: ensureClusterRoles: %v", name, err)
		}
		if got := clusterRoleBindings(t, r); len(got) != 0 {
			t.Fatalf("%s: a role was bound: %+v", name, got[0])
		}
		cond := clusterRolesCondition(comp)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason {
			t.Fatalf("%s: condition = %+v, want False/%s", name, cond, tc.reason)
		}
		if !strings.Contains(cond.Message, c.request.Name) {
			t.Fatalf("%s: the message does not name the role: %q", name, cond.Message)
		}
	}
}

// A role that carries the set's name and not the platform's label is not the
// platform's, and is not bound.
func TestAClusterRoleWithoutThePlatformsLabelIsNotBound(t *testing.T) {
	c := clusterRoleEverythingInPlace()
	c.roleThere = false
	r, comp, profile := c.build(t)
	if err := r.Create(context.Background(), &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: security.ClusterRoleObjectName("read-nodes")},
	}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := r.ensureClusterRoles(context.Background(), comp, profile); err != nil {
		t.Fatalf("ensureClusterRoles: %v", err)
	}
	if got := clusterRoleBindings(t, r); len(got) != 0 {
		t.Fatalf("a role that is not the platform's was bound: %+v", got[0])
	}
}

// Withdrawing the grant, or the cluster's permission, takes the binding away
// at the next reconcile. So does the component going.
func TestAClusterRoleBindingGoesWithWhatAllowedIt(t *testing.T) {
	ctx := context.Background()
	withdrawals := map[string]func(*ComponentReconciler, *gentianov1alpha1.Component, *gentianov1alpha1.ComponentProfile) error{
		"the grant is withdrawn": func(r *ComponentReconciler, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
			comp.Spec.Privileges = nil
			return r.ensureClusterRoles(ctx, comp, profile)
		},
		"the cluster no longer permits it": func(r *ComponentReconciler, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
			psp := &gentianov1alpha1.PlatformSecurityPolicy{}
			if err := r.Get(ctx, client.ObjectKey{Name: gentianov1alpha1.PlatformSecurityPolicyName}, psp); err != nil {
				return err
			}
			psp.Spec.AllowedClusterRoles = nil
			if err := r.Update(ctx, psp); err != nil {
				return err
			}
			return r.ensureClusterRoles(ctx, comp, profile)
		},
		"the profile no longer asks for it": func(r *ComponentReconciler, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
			profile.Spec.Requires.Privileges.ClusterRoles = nil
			return r.ensureClusterRoles(ctx, comp, profile)
		},
		"the component goes": func(r *ComponentReconciler, comp *gentianov1alpha1.Component, _ *gentianov1alpha1.ComponentProfile) error {
			return r.deleteClusterRoles(ctx, comp)
		},
	}
	for name, withdraw := range withdrawals {
		r, comp, profile := clusterRoleEverythingInPlace().build(t)
		// Another component's binding, which none of this may touch.
		other := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: "other", Labels: map[string]string{
				managedByLabel: managedByValue, clusterRoleBindingNamespaceLabel: "tenant-demo",
				clusterRoleBindingComponentLabel: "another",
			}},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "gentian-profile-role-read-nodes"},
		}
		if err := r.Create(ctx, other); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		if err := r.ensureClusterRoles(ctx, comp, profile); err != nil {
			t.Fatalf("%s: ensureClusterRoles: %v", name, err)
		}
		if got := clusterRoleBindings(t, r); len(got) != 2 {
			t.Fatalf("%s: bindings = %d before the withdrawal, want the component's and the other", name, len(got))
		}
		if err := withdraw(r, comp, profile); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := clusterRoleBindings(t, r)
		if len(got) != 1 || got[0].Name != "other" {
			t.Fatalf("%s: bindings left = %+v, want only the other component's", name, got)
		}
	}
}

// A binding that was changed by hand to another subject is not what was
// approved: it is replaced by the one that was.
func TestAClusterRoleBindingChangedByHandIsReplaced(t *testing.T) {
	ctx := context.Background()
	r, comp, profile := clusterRoleEverythingInPlace().build(t)
	if err := r.ensureClusterRoles(ctx, comp, profile); err != nil {
		t.Fatalf("ensureClusterRoles: %v", err)
	}
	b := clusterRoleBindings(t, r)[0]
	b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "intruder", Namespace: "tenant-other"})
	if err := r.Update(ctx, &b); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Two reconciles: the first removes what is not as approved, the second
	// writes it as approved.
	for range 2 {
		if err := r.ensureClusterRoles(ctx, comp, profile); err != nil {
			t.Fatalf("ensureClusterRoles: %v", err)
		}
	}
	got := clusterRoleBindings(t, r)
	if len(got) != 1 || len(got[0].Subjects) != 1 || got[0].Subjects[0].Name != "dashboard" {
		t.Fatalf("bindings = %+v, want the approved one alone", got)
	}
}

// The platform's own set is empty, so on a build as it ships nothing is
// bound whatever a profile asks, the cluster permits and an officer granted
// -- and the API server is not asked for a binding at all.
func TestThePlatformsOwnSetBindsNothing(t *testing.T) {
	if len(security.PlatformClusterRoles) != 0 {
		t.Skip("the platform ships cluster roles; this is the test of the empty set")
	}
	r, comp, profile := clusterRoleEverythingInPlace().build(t)
	r.ClusterRoles = nil
	if err := r.ensureClusterRoles(context.Background(), comp, profile); err != nil {
		t.Fatalf("ensureClusterRoles: %v", err)
	}
	if got := clusterRoleBindings(t, r); len(got) != 0 {
		t.Fatalf("a role was bound from an empty set: %+v", got[0])
	}
	if cond := clusterRolesCondition(comp); cond == nil || cond.Reason != security.ClusterRoleUnknown {
		t.Fatalf("condition = %+v, want %s", cond, security.ClusterRoleUnknown)
	}
}
