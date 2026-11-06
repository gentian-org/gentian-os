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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func externalSecretWithReady(status string, since time.Time, annotations map[string]string) *unstructured.Unstructured {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	es.SetName("app-values")
	es.SetNamespace("tenant-a")
	es.SetAnnotations(annotations)
	es.Object["status"] = map[string]any{"conditions": []any{map[string]any{
		"type": "Ready", "status": status, "reason": "SecretSyncedError",
		"lastTransitionTime": since.UTC().Format(time.RFC3339),
	}}}
	return es
}

// A failed ExternalSecret is read again once it has been failed for
// externalSecretRetryAfter, and again each time that has passed since the
// last retry; one that is ready, or failed only a moment ago, is left alone.
func TestAFailedExternalSecretIsReadAgain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	key := types.NamespacedName{Name: "app-values", Namespace: "tenant-a"}
	retried := func(r *ExternalSecretRetryReconciler) string {
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(secrets.ExternalSecretGVK)
		if err := r.Get(ctx, key, es); err != nil {
			t.Fatal(err)
		}
		return es.GetAnnotations()[syncRetriedAnnotation]
	}
	stamp := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

	for _, tc := range []struct {
		name      string
		es        *unstructured.Unstructured
		want      string
		wantAfter time.Duration
	}{
		{"ready", externalSecretWithReady("True", now.Add(-time.Hour), nil), "", 0},
		{"failed a moment ago", externalSecretWithReady("False", now.Add(-time.Minute), nil), "", 9 * time.Minute},
		{"failed long ago", externalSecretWithReady("False", now.Add(-32*time.Hour), map[string]string{"force-sync": "1"}), stamp(now), externalSecretRetryAfter},
		{"retried a moment ago", externalSecretWithReady("False", now.Add(-time.Hour),
			map[string]string{syncRetriedAnnotation: stamp(now.Add(-2 * time.Minute))}), stamp(now.Add(-2 * time.Minute)), 8 * time.Minute},
		{"retried long ago", externalSecretWithReady("False", now.Add(-time.Hour),
			map[string]string{syncRetriedAnnotation: stamp(now.Add(-20 * time.Minute))}), stamp(now), externalSecretRetryAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).WithObjects(tc.es).Build()
			r := &ExternalSecretRetryReconciler{Client: c, Now: func() time.Time { return now }}
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatal(err)
			}
			if got := retried(r); got != tc.want {
				t.Errorf("%s = %q, want %q", syncRetriedAnnotation, got, tc.want)
			}
			if res.RequeueAfter != tc.wantAfter {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.wantAfter)
			}
		})
	}
}
