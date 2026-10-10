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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

const (
	appGrantFinalizer      = "gentianos.io/app-grant-cleanup"
	conditionAppGrantReady = "AppGrantReady"
)

// +kubebuilder:rbac:groups=gentianos.io,resources=appgrants,verbs=get;list;watch;update;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appgrants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appgrants/finalizers,verbs=update

// AppGrantReconciler records an AppGrant as admitted.
//
// It writes nothing to the authorization store. The director is the store's
// only writer and projects grants from git when it starts, so a second writer
// here would delete on its own schedule what the director wrote on its own.
// What remains is the object's lifecycle: a finalizer so a grant is
// observed leaving, and a status that says it was taken in.
type AppGrantReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
}

func (r *AppGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	grant := &gentianov1alpha1.AppGrant{}
	if err := r.Get(ctx, req.NamespacedName, grant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !grant.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(grant, appGrantFinalizer) {
			controllerutil.RemoveFinalizer(grant, appGrantFinalizer)
			return ctrl.Result{}, r.Update(ctx, grant)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(grant, appGrantFinalizer) {
		controllerutil.AddFinalizer(grant, appGrantFinalizer)
		if err := r.Update(ctx, grant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if grant.Status.Phase == gentianov1alpha1.AppGrantPhaseReady && grant.Status.ObservedGeneration == grant.Generation {
		return ctrl.Result{}, nil
	}
	setAppGrantCondition(grant, conditionAppGrantReady, metav1.ConditionTrue, "Recorded",
		"grant recorded; the director projects it to the authorization store from git")
	grant.Status.Phase = gentianov1alpha1.AppGrantPhaseReady
	grant.Status.ObservedGeneration = grant.Generation
	return ctrl.Result{}, r.Status().Update(ctx, grant)
}

func (r *AppGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.AppGrant{}).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.AppGrant{}, r))
}

func setAppGrantCondition(grant *gentianov1alpha1.AppGrant, typ string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range grant.Status.Conditions {
		if grant.Status.Conditions[i].Type == typ {
			grant.Status.Conditions[i].Status = status
			grant.Status.Conditions[i].Reason = reason
			grant.Status.Conditions[i].Message = message
			grant.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	grant.Status.Conditions = append(grant.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// The tenant reconciler writes no AppGrant.
//
// What a profile lists under integrations is a request. A grant is the
// answer, and the answer is an administrator's: the director commits it for
// somebody who may grant in the tenant. A reconciler that wrote a grant from
// the profile would answer the request with the request, and one that kept
// its copy in step with the profile would put back what an administrator
// took away. So a declared capability nobody granted stays closed, and the
// binding says so in its status (conditionBindingGranted).

// deleteAppGrants removes, as a tenant is taken down, the grants that carry
// the operator's label. Nothing else here deletes one.
func (r *TenantReconciler) deleteAppGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	existing := &gentianov1alpha1.AppGrantList{}
	if err := r.List(ctx, existing,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue},
	); err != nil {
		return fmt.Errorf("list AppGrants in %s: %w", nsName, err)
	}
	for i := range existing.Items {
		if err := r.Delete(ctx, &existing.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete AppGrant %s: %w", existing.Items[i].Name, err)
		}
	}
	return nil
}
