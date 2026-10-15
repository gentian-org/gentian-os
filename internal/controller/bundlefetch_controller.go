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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// BundleFetchReconciler closes a fetch's way out of the cluster when the
// fetch ends. The way out is a NetworkPolicy the operator's API made with the
// fetch's Job, owned by it (applifecycle.StartFetch); it would go with the Job
// an hour later, and a way out is not left open for an hour after nothing
// needs it.
type BundleFetchReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;delete

func (r *BundleFetchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, req.NamespacedName, job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !jobEnded(job) {
		return ctrl.Result{}, nil
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: backup.FetchPolicyName(job.Name), Namespace: job.Namespace,
	}}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// jobEnded reports whether a Job completed or failed for good.
func jobEnded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager registers the reconciler for fetch Jobs alone.
func (r *BundleFetchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.Job{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetNamespace() == s3Namespace &&
				obj.GetLabels()[meta.ComponentLabel] == backup.FetchComponent &&
				obj.GetLabels()[backup.FetchLabel] != ""
		}))).
		Named("bundlefetch").
		Complete(r)
}
