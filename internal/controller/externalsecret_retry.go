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
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// externalSecretRetryAfter is how long an ExternalSecret stays failed before
// the operator has the secrets operator read it again, and how long between
// one such retry and the next.
const externalSecretRetryAfter = 10 * time.Minute

// syncRetriedAnnotation holds when the operator last had a failed
// ExternalSecret read again, in Unix seconds. Not force-sync: the credential
// catalog reads a fresh force-sync stamp as a value just stored and still
// pending, which a retry is not.
const syncRetriedAnnotation = "gentianos.io/sync-retried"

// ExternalSecretRetryReconciler has a failed ExternalSecret read again.
//
// The secrets operator is meant to retry a failed read on its own. On one
// cluster it did not: the cluster's DNS was gone for a quarter of an
// hour, a dozen ExternalSecrets failed in that time, and none of them was read
// again for a day and a half -- not on a retry, not on its hourly refresh --
// while every store they read from was ready. Their Secrets still held the
// old values, so nothing stopped; but every app reading one reported its
// values pending, the operator's own Application read as degraded, and the
// installer stopped on it.
//
// Any change to an ExternalSecret's annotations makes the secrets operator
// read it again, so a failure that outlasts externalSecretRetryAfter gets one
// such change, and another each time that has passed again. A store still
// down fails the read again, and the next retry waits its turn; a value that
// is really missing is asked for once every ten minutes, which costs nothing.
type ExternalSecretRetryReconciler struct {
	client.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;patch

func (r *ExternalSecretRetryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ExternalSecretRetryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	if err := r.Get(ctx, req.NamespacedName, es); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	failedSince, failed := externalSecretFailedSince(es)
	if !failed {
		return ctrl.Result{}, nil
	}
	since := failedSince
	if stamp, err := strconv.ParseInt(es.GetAnnotations()[syncRetriedAnnotation], 10, 64); err == nil {
		if retried := time.Unix(stamp, 0); retried.After(since) {
			since = retried
		}
	}
	now := r.now()
	if wait := since.Add(externalSecretRetryAfter).Sub(now); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	// A merge patch of the one annotation: the rest of the object is its
	// owner's to write.
	before := es.DeepCopy()
	annotations := es.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[syncRetriedAnnotation] = strconv.FormatInt(now.Unix(), 10)
	es.SetAnnotations(annotations)
	if err := r.Patch(ctx, es, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctrl.LoggerFrom(ctx).Info("had a failed ExternalSecret read again", "failedSince", failedSince.UTC().Format(time.RFC3339))
	return ctrl.Result{RequeueAfter: externalSecretRetryAfter}, nil
}

// externalSecretFailedSince is when an ExternalSecret's Ready condition
// turned False, and whether it is False now.
func externalSecretFailedSince(es *unstructured.Unstructured) (time.Time, bool) {
	conditions, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		if cond["status"] != "False" {
			return time.Time{}, false
		}
		at, _ := cond["lastTransitionTime"].(string)
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			// A failure with no time on it is retried now.
			return time.Time{}, true
		}
		return t, true
	}
	return time.Time{}, false
}

func (r *ExternalSecretRetryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(es).
		Named("externalsecret-retry").
		Complete(r)
}
