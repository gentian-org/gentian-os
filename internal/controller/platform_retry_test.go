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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// unreadableClient is a fake client whose every read fails, as it does while
// the API server or a CRD is not there yet.
func unreadableClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("read refused")
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return refused
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return refused
		},
	}).Build()
}

// The cluster-wide Gateway and Keycloak reconcilers retry a failed step after
// platformRetryAfter. The delay is returned with a nil error: beside an error
// controller-runtime would drop it and fall back to its own backoff.
func TestPlatformReconcilersRetryAFailedStepAfterTheirDelay(t *testing.T) {
	t.Parallel()

	reconcilers := map[string]reconcile.Reconciler{
		"gateway":  &GatewayPlatformReconciler{Client: unreadableClient(t), KernelDomain: "example.test", KernelRealm: "kernel"},
		"keycloak": &KeycloakPlatformReconciler{Client: unreadableClient(t), KernelDomain: "example.test", KernelRealm: "kernel"},
	}
	for name, r := range reconcilers {
		res, err := r.Reconcile(context.Background(), reconcile.Request{})
		if err != nil {
			t.Errorf("%s: error %v returned; the failure is logged and retried after platformRetryAfter", name, err)
		}
		if res.RequeueAfter != platformRetryAfter {
			t.Errorf("%s: RequeueAfter = %s, want %s", name, res.RequeueAfter, platformRetryAfter)
		}
	}
}

// The authorization table waits for its namespace with a delay and a nil
// error, and returns any other read failure alone for controller-runtime's
// backoff.
func TestAuthorizationTableWaitsForItsNamespaceAndReturnsOtherFailures(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	absent := &AuthorizationTableReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	res, err := absent.Reconcile(context.Background(), authorizationTableRequest)
	if err != nil || res.RequeueAfter != componentRequeue {
		t.Fatalf("namespace absent: Result %+v, error %v; want RequeueAfter %s and no error", res, err, componentRequeue)
	}

	res, err = (&AuthorizationTableReconciler{Client: unreadableClient(t)}).Reconcile(context.Background(), authorizationTableRequest)
	if err == nil || res != (reconcile.Result{}) {
		t.Fatalf("read refused: Result %+v, error %v; want the error alone", res, err)
	}
}
