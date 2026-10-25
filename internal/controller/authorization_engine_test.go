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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/arbiter"
)

// While the engine is wanted: the instance's datastore Secret and role Job
// first, and once the role is made the arbiter, its narrow Role and the two
// policies. Nothing at all while it is not.
func TestTheAuthorizationEngineIsMadeWhileWanted(t *testing.T) {
	ctx := context.Background()
	set := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"},
		Data: map[string]string{EngineAuthorization: enginePurge}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: arbiter.Namespace}}
	c := fake.NewClientBuilder().WithScheme(rightsScheme(t)).WithObjects(set, ns).WithStatusSubresource(&batchv1.Job{}).Build()
	r := &AuthorizationEngineReconciler{Client: c, Namespace: "kernel-control", Image: "ghcr.io/x/gentian-os:develop-abc"}
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.DatastoreSecret, Namespace: arbiter.Namespace}, &corev1.Secret{}); err == nil {
		t.Fatal("a purged engine got a datastore")
	}

	set.Data[EngineAuthorization] = engineWanted
	if err := c.Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	ds := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.DatastoreSecret, Namespace: arbiter.Namespace}, ds); err != nil {
		t.Fatal(err)
	}
	pw := string(ds.Data["password"])
	if len(pw) < 30 || !strings.Contains(string(ds.Data["uri"]), "openfga_apps:"+pw+"@postgres-rw.system-postgresql") {
		t.Fatalf("datastore = %v", ds.Data)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiterRoleJob, Namespace: "system-postgresql"}, job); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.ServiceName, Namespace: arbiter.Namespace}, &appsv1.Deployment{}); err == nil {
		t.Fatal("the arbiter came before the instance's role")
	}

	job.Status.Succeeded = 1
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.ServiceName, Namespace: arbiter.Namespace}, dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.Containers[0]; got.Image != r.Image || got.Command[0] != "/arbiter" {
		t.Fatalf("container = %s %v", got.Image, got.Command)
	}
	role := &rbacv1.Role{}
	if err := c.Get(ctx, types.NamespacedName{Name: arbiter.ServiceName, Namespace: arbiter.Namespace}, role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			if verb != "create" && len(rule.ResourceNames) == 0 {
				t.Fatalf("the arbiter may %s every %v", verb, rule.Resources)
			}
		}
	}
	instance := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "openfga-apps-ingress", Namespace: arbiter.Namespace}, instance); err != nil {
		t.Fatal(err)
	}
	if from := instance.Spec.Ingress[0].From; len(from) != 1 || from[0].NamespaceSelector != nil || from[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "arbiter" {
		t.Fatalf("the instance admits %+v", from)
	}
	// The password is kept.
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil || string(ds.Data["password"]) != pw {
		t.Fatal("the datastore's password changed")
	}
}
