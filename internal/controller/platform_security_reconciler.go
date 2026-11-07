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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
	"github.com/gentian-org/gentian-os/internal/security"
)

const (
	conditionPlatformSecurityReady = "PlatformSecurityReady"
	platformSecurityRequeue        = 5 * time.Minute
)

// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=platformsecuritypolicies/finalizers,verbs=update

// PlatformSecurityPolicyReconciler syncs cluster MAC waiver allowlist to a ConfigMap.
type PlatformSecurityPolicyReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	OperatorNamespace string
}

func (r *PlatformSecurityPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if req.Name != gentianov1alpha1.PlatformSecurityPolicyName {
		return ctrl.Result{}, nil
	}

	psp := &gentianov1alpha1.PlatformSecurityPolicy{}
	if err := r.Get(ctx, req.NamespacedName, psp); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.ensureDefaultPolicy(ctx); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	if err := security.SyncPlatformSecurityConfigMap(ctx, r.Client, psp.Spec.AllowedMacWaivers); err != nil {
		setStatusCondition(&psp.Status.Conditions, psp.Generation, conditionPlatformSecurityReady, metav1.ConditionFalse, "SyncFailed", err.Error())
		if updateErr := r.Status().Update(ctx, psp); updateErr != nil {
			return ctrl.Result{}, errors.Join(err, fmt.Errorf("record the sync failure on the policy: %w", updateErr))
		}
		return ctrl.Result{}, err
	}

	psp.Status.AllowedMacWaiverCount = len(psp.Spec.AllowedMacWaivers)
	setStatusCondition(&psp.Status.Conditions, psp.Generation, conditionPlatformSecurityReady, metav1.ConditionTrue, "Synced",
		"Platform security ConfigMap updated")
	if err := r.Status().Update(ctx, psp); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("platform security policy synced", "waivers", psp.Status.AllowedMacWaiverCount)
	return ctrl.Result{RequeueAfter: platformSecurityRequeue}, nil
}

func (r *PlatformSecurityPolicyReconciler) ensureDefaultPolicy(ctx context.Context) error {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec:       gentianov1alpha1.PlatformSecurityPolicySpec{},
	}
	if err := r.Create(ctx, psp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create default PlatformSecurityPolicy: %w", err)
	}
	return nil
}

func (r *PlatformSecurityPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.PlatformSecurityPolicy{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.PlatformSecurityPolicy{}, r))
}
