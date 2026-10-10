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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// The cluster roles of a component.
//
// A profile asks for a cluster role by name. The platform defines the roles
// (internal/security/cluster_roles.go, and the ClusterRole objects the
// operator's chart ships); a profile's own rules are never made into one.
// What is written here is a ClusterRoleBinding of one of those roles to the
// ServiceAccount the profile names, in the component's namespace -- and only
// while the platform has the role, the cluster's PlatformSecurityPolicy
// permits it for the profile, and a live grant for it is on the install.
// When one of the three goes, the binding goes at the next reconcile.
//
// The operator's own role carries no permission on RBAC objects while the
// platform's set is empty, and with an empty set nothing here asks the API
// server for one. The permissions arrive with the first role, and the
// permission to bind is held to the names of the set (docs/design/security.md
// says how a role is added).

const (
	// conditionClusterRolesBound says, on a Component whose profile asks for
	// cluster roles, which were bound and why each of the others was not.
	conditionClusterRolesBound = "ClusterRolesBound"

	clusterRoleBindingNamespaceLabel = "gentianos.io/component-namespace"
	clusterRoleBindingComponentLabel = "gentianos.io/component"
)

// clusterRoleSet is the platform's set, or the one a test put in its place.
func (r *ComponentReconciler) clusterRoleSet() security.ClusterRoleSet {
	if r.ClusterRoles != nil {
		return r.ClusterRoles
	}
	return security.PlatformClusterRoles
}

// clusterRoleBindingName is one binding's name: the component and the role
// readable in it, and a hash of the three parts so that two components can
// never share one.
func clusterRoleBindingName(namespace, component, role string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + component + "\x00" + role))
	readable := namespace + "-" + component + "-" + role
	if len(readable) > 180 {
		readable = readable[:180]
	}
	return security.ClusterRoleObjectPrefix + readable + "-" + hex.EncodeToString(sum[:5])
}

// ensureClusterRoles binds what may be bound, removes what may no longer be,
// and writes the condition that says which is which. It does not save the
// status: every path of the reconciler that follows does.
func (r *ComponentReconciler) ensureClusterRoles(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	var requests []gentianov1alpha1.ClusterRoleRequest
	if profile != nil && profile.Privileges() != nil {
		requests = profile.Privileges().ClusterRoles
	}
	set := r.clusterRoleSet()
	if len(set) == 0 {
		// No role exists, so no binding can: nothing to make and nothing to
		// remove, and the API server is not asked.
		r.setClusterRolesCondition(comp, security.ResolveClusterRoles(profile, set, nil, comp.Spec.Privileges, time.Now()))
		return nil
	}

	var verdicts []security.ClusterRoleVerdict
	if len(requests) > 0 {
		allowed, err := security.LoadAllowedClusterRoles(ctx, r.Client)
		if err != nil {
			return err
		}
		verdicts = security.ResolveClusterRoles(profile, set, allowed, comp.Spec.Privileges, time.Now())
	}

	desired := map[string]*rbacv1.ClusterRoleBinding{}
	for i := range verdicts {
		v := &verdicts[i]
		if !v.Bind {
			continue
		}
		// The role as the cluster has it. A name in the set whose object is
		// not on this cluster, or is not the platform's, binds nothing.
		role := &rbacv1.ClusterRole{}
		err := r.Get(ctx, client.ObjectKey{Name: security.ClusterRoleObjectName(v.Name)}, role)
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("get ClusterRole %s: %w", security.ClusterRoleObjectName(v.Name), err)
		}
		if err != nil || role.Labels[security.ClusterRoleNameLabel] != v.Name {
			v.Bind = false
			v.Reason = "RoleNotInstalled"
			v.Message = v.Name + ": the platform's ClusterRole " + security.ClusterRoleObjectName(v.Name) + " is not on this cluster"
			continue
		}
		name := clusterRoleBindingName(comp.Namespace, comp.Name, v.Name)
		desired[name] = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					managedByLabel:                   managedByValue,
					clusterRoleBindingNamespaceLabel: comp.Namespace,
					clusterRoleBindingComponentLabel: comp.Name,
					security.ClusterRoleNameLabel:    v.Name,
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName, Kind: "ClusterRole",
				Name: security.ClusterRoleObjectName(v.Name),
			},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: v.ServiceAccount, Namespace: comp.Namespace,
			}},
		}
	}

	existing, err := r.componentClusterRoleBindings(ctx, comp)
	if err != nil {
		return err
	}
	for i := range existing {
		have := &existing[i]
		want, keep := desired[have.Name]
		// A binding that is no longer wanted goes. So does one whose subject
		// or role differs from what is wanted: neither can be changed in
		// place into what was approved, and the role of a binding cannot be
		// changed at all.
		if keep && have.RoleRef == want.RoleRef && len(have.Subjects) == 1 && have.Subjects[0] == want.Subjects[0] {
			delete(desired, have.Name)
			continue
		}
		if err := r.Delete(ctx, have); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", have.Name, err)
		}
	}
	for _, binding := range desired {
		if err := r.Create(ctx, binding); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create ClusterRoleBinding %s: %w", binding.Name, err)
		}
	}
	r.setClusterRolesCondition(comp, verdicts)
	return nil
}

// deleteClusterRoles removes every binding of a component that is going.
func (r *ComponentReconciler) deleteClusterRoles(ctx context.Context, comp *gentianov1alpha1.Component) error {
	if len(r.clusterRoleSet()) == 0 {
		return nil
	}
	existing, err := r.componentClusterRoleBindings(ctx, comp)
	if err != nil {
		return err
	}
	for i := range existing {
		if err := r.Delete(ctx, &existing[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", existing[i].Name, err)
		}
	}
	return nil
}

func (r *ComponentReconciler) componentClusterRoleBindings(ctx context.Context, comp *gentianov1alpha1.Component) ([]rbacv1.ClusterRoleBinding, error) {
	list := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(ctx, list, client.MatchingLabels{
		managedByLabel:                   managedByValue,
		clusterRoleBindingNamespaceLabel: comp.Namespace,
		clusterRoleBindingComponentLabel: comp.Name,
	}); err != nil {
		return nil, fmt.Errorf("list ClusterRoleBindings of %s/%s: %w", comp.Namespace, comp.Name, err)
	}
	return list.Items, nil
}

// setClusterRolesCondition says what became of each request. No request, no
// condition.
func (r *ComponentReconciler) setClusterRolesCondition(comp *gentianov1alpha1.Component, verdicts []security.ClusterRoleVerdict) {
	if len(verdicts) == 0 {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, conditionClusterRolesBound)
		return
	}
	messages := make([]string, 0, len(verdicts))
	reason := security.ClusterRoleBound
	for _, v := range verdicts {
		messages = append(messages, v.Message)
		if !v.Bind && reason == security.ClusterRoleBound {
			reason = v.Reason
		}
	}
	sort.Strings(messages)
	status := metav1.ConditionTrue
	if reason != security.ClusterRoleBound {
		status = metav1.ConditionFalse
	}
	apimeta.SetStatusCondition(&comp.Status.Conditions, metav1.Condition{
		Type: conditionClusterRolesBound, Status: status, Reason: reason,
		Message: strings.Join(messages, "; "), ObservedGeneration: comp.Generation,
	})
}

// componentsAskingForClusterRoles wakes, when the cluster's security policy
// changes, every component whose profile asks for a cluster role.
func componentsAskingForClusterRoles(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		asking := map[string]struct{}{}
		for i := range profiles.Items {
			if p := profiles.Items[i].Privileges(); p != nil && len(p.ClusterRoles) > 0 {
				asking[profiles.Items[i].Name] = struct{}{}
			}
		}
		if len(asking) == 0 {
			return nil
		}
		comps := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, comps); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range comps.Items {
			if _, ok := asking[comps.Items[i].Spec.ProfileRef.Name]; ok {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
			}
		}
		return out
	})
}
