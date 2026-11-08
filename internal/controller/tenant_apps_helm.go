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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// The account a tenant's apps are installed as.
//
// provider-helm installs the kernel's charts as cluster-admin, which some of
// them need. A tenant's chart is installed through the ProviderConfig
// tenant-apps instead, whose kubeconfig makes provider-helm act as the
// ServiceAccount provider-helm-tenant-apps (crossplane/providers/). That
// account holds nothing cluster-wide; this reconciler gives it the ClusterRole
// gentian-tenant-apps-helm in each tenant namespace, by a RoleBinding there,
// and nowhere else. So a tenant chart can create, change and delete the
// namespaced kinds that role names in its own namespace, and nothing in
// another namespace or at cluster scope.
//
// The binding outlives its namespace's other objects. Uninstalling a release
// needs the account's rights in the namespace, and a namespace being deleted
// removes its RoleBindings with everything else, so a release deleted with its
// tenant could not be uninstalled and would hold its finalizer for good. The
// binding carries a finalizer that comes off once no Release installs into
// the namespace any more.
const (
	// TenantAppsProviderConfig is the provider-helm ProviderConfig a tenant's
	// Releases name.
	TenantAppsProviderConfig = "tenant-apps"
	// tenantAppsHelmServiceAccount is the account it acts as, in the
	// provisioning namespace.
	tenantAppsHelmServiceAccount = "provider-helm-tenant-apps"
	// tenantAppsHelmRole is the ClusterRole bound, and the binding's name.
	tenantAppsHelmRole = "gentian-tenant-apps-helm"
	// tenantAppsHelmFinalizer keeps the binding while a Release still
	// installs into its namespace.
	tenantAppsHelmFinalizer = "gentianos.io/tenant-app-releases"
	// tenantAppsHelmRequeue is how soon a namespace being deleted is looked
	// at again for Releases still in it. A Release is not watched.
	tenantAppsHelmRequeue = 10 * time.Second
)

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment.
//
// Binding is held to the one role by name. Creating a RoleBinding is
// otherwise allowed only for a role whose every permission the operator holds
// itself (escalation prevention).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=gentian-tenant-apps-helm
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=helm.crossplane.io,resources=releases,verbs=get;list;watch

// TenantAppsHelmReconciler keeps the account tenant apps are installed as
// bound in every tenant namespace.
type TenantAppsHelmReconciler struct {
	client.Client
}

// tenantAppsNamespace reports whether a namespace is one a tenant's apps are
// installed into: in the tenant tier and named as a tenant's namespace. The
// tier label is set by the platform alone (namespace-tier-guard.yaml); the
// name keeps the binding off any namespace a Tenant could name for itself
// that is not tenant-*.
func tenantAppsNamespace(ns *corev1.Namespace) bool {
	return ns.Labels[layout.LabelTier] == string(layout.TierTenant) && strings.HasPrefix(ns.Name, "tenant-")
}

// desiredTenantAppsBinding is the binding in one namespace.
func desiredTenantAppsBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:       tenantAppsHelmRole,
			Namespace:  namespace,
			Labels:     map[string]string{managedByLabel: managedByValue},
			Finalizers: []string{tenantAppsHelmFinalizer},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      tenantAppsHelmServiceAccount,
			Namespace: layout.Kernel(layout.Provisioning),
		}},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tenantAppsHelmRole},
	}
}

func (r *TenantAppsHelmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tenantAppsNamespace(ns) {
		return ctrl.Result{}, nil
	}
	have := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: tenantAppsHelmRole}, have)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	found := err == nil

	// The namespace is going: the binding stays until nothing is left to
	// uninstall in it, then lets the namespace go.
	if ns.DeletionTimestamp != nil {
		if !found || !controllerutil.ContainsFinalizer(have, tenantAppsHelmFinalizer) {
			return ctrl.Result{}, nil
		}
		left, err := r.releasesInto(ctx, ns.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if left > 0 {
			return ctrl.Result{RequeueAfter: tenantAppsHelmRequeue}, nil
		}
		return ctrl.Result{}, r.release(ctx, have)
	}

	want := desiredTenantAppsBinding(ns.Name)
	switch {
	case !found:
		return ctrl.Result{}, r.Create(ctx, want)
	case have.DeletionTimestamp != nil, !equality.Semantic.DeepEqual(have.RoleRef, want.RoleRef):
		// Deleted by someone while the namespace stays, or bound to another
		// role, which a binding cannot be moved off: it goes and is made
		// again on the next pass.
		if err := r.release(ctx, have); err != nil {
			return ctrl.Result{}, err
		}
		if have.DeletionTimestamp == nil {
			if err := r.Delete(ctx, have); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case !equality.Semantic.DeepEqual(have.Subjects, want.Subjects),
		!controllerutil.ContainsFinalizer(have, tenantAppsHelmFinalizer),
		have.Labels[managedByLabel] != managedByValue:
		patch := client.MergeFrom(have.DeepCopy())
		have.Subjects = want.Subjects
		controllerutil.AddFinalizer(have, tenantAppsHelmFinalizer)
		if have.Labels == nil {
			have.Labels = map[string]string{}
		}
		have.Labels[managedByLabel] = managedByValue
		return ctrl.Result{}, r.Patch(ctx, have, patch)
	}
	return ctrl.Result{}, nil
}

// release takes the finalizer off a binding.
func (r *TenantAppsHelmReconciler) release(ctx context.Context, b *rbacv1.RoleBinding) error {
	if !controllerutil.ContainsFinalizer(b, tenantAppsHelmFinalizer) {
		return nil
	}
	patch := client.MergeFrom(b.DeepCopy())
	controllerutil.RemoveFinalizer(b, tenantAppsHelmFinalizer)
	if err := r.Patch(ctx, b, patch); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("release the binding in %s: %w", b.Namespace, err)
	}
	return nil
}

// releasesInto counts the Releases that install into a namespace, whichever
// ProviderConfig they name.
func (r *TenantAppsHelmReconciler) releasesInto(ctx context.Context, namespace string) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseGVK.GroupVersion().WithKind(helmReleaseGVK.Kind + "List"))
	if err := r.List(ctx, list); err != nil {
		return 0, fmt.Errorf("list Helm Releases: %w", err)
	}
	n := 0
	for i := range list.Items {
		if target, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "forProvider", "namespace"); target == namespace {
			n++
		}
	}
	return n, nil
}

func (r *TenantAppsHelmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toNamespace := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if o.GetName() != tenantAppsHelmRole {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetNamespace()}}}
	})
	tenantTier := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[layout.LabelTier] == string(layout.TierTenant)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("tenant-apps-helm").
		For(&corev1.Namespace{}, builder.WithPredicates(tenantTier)).
		Watches(&rbacv1.RoleBinding{}, toNamespace).
		Complete(r)
}
