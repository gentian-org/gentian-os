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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const tenantShellRequeueAfter = 5 * time.Second

// waitForTenantShell blocks until Crossplane (tenant-default Composition) has
// created the tenant namespace and it is not terminating.
func (r *TenantReconciler) waitForTenantShell(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) (ctrl.Result, error) {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
	if errors.IsNotFound(err) {
		r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionFalse, "WaitingForCrossplane",
			fmt.Sprintf("waiting for Crossplane to provision namespace %q", nsName))
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if ns.DeletionTimestamp != nil {
		r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionFalse, "NamespaceTerminating",
			fmt.Sprintf("namespace %q is still terminating", nsName))
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}
