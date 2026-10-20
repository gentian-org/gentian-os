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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// ProfileBuildReconciler reads each ProfileBuild's bundle and writes what it
// found on the status: the profile's spec, with the defaults the API server
// gives a ComponentProfile, when the bundle is the digest it claims and
// holds what a bundle may; nothing, and why, when it is not.
//
// The status is what the app Composition reads for a tenant on that build,
// because a Composition can select an object and read its fields and can do
// nothing else: it cannot decode a bundle or apply a schema's defaults. The
// operator can, and does it once per build rather than once per render.
type ProfileBuildReconciler struct {
	// Definitions holds this reconciler while the cluster's definitions
	// would drop what it writes (the status of a kind this build adds). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
}

const profileBuildVerified = "Verified"

// +kubebuilder:rbac:groups=gentianos.io,resources=profilebuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=profilebuilds/status,verbs=get;update;patch

// Reconcile writes the build's status.
func (r *ProfileBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	build := &gentianov1alpha1.ProfileBuild{}
	if err := r.Get(ctx, req.NamespacedName, build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := build.DeepCopy()
	condition := metav1.Condition{Type: profileBuildVerified, ObservedGeneration: build.Generation}
	profile, _, refusal := profilebundle.FromBuild(build)
	switch {
	case refusal != nil:
		condition.Status = metav1.ConditionFalse
		condition.Reason = refusal.Reason
		condition.Message = refusal.Message + "; no tenant is rendered from this build"
		build.Status.ProfileSpec = nil
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "BundleVerified"
		condition.Message = "the bundle is " + profilebundle.Short(build.Spec.Digest) + " and its profile is on the status"
		spec := profile.Spec
		build.Status.ProfileSpec = &spec
	}
	meta.SetStatusCondition(&build.Status.Conditions, condition)
	if equality.Semantic.DeepEqual(before.Status, build.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, build, client.MergeFrom(before)); err != nil {
		if errors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *ProfileBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("profilebuild").
		For(&gentianov1alpha1.ProfileBuild{}).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.ProfileBuild{}, r))
}
