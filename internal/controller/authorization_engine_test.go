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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// While the engine is wanted: the instance's datastore Secret and role Job
// first, and once the role is made the arbiter, its narrow Role and the two
// policies. Nothing at all while it is not.
func TestTheAuthorizationEngineIsMadeWhileWanted(t *testing.T) {
	ctx := context.Background()
	set := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"},
		Data: map[string]string{EngineAuthorization: enginePurge}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: arbiter.Namespace}}
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).WithObjects(set, ns).WithStatusSubresource(&batchv1.Job{}).Build()
	vault := newSecretVault()
	seeder := secrets.NewSeeder(vault, secrets.NewDeriver("a master password", "a salt")).WithMode(randomMode)
	r := &AuthorizationEngineReconciler{Client: c, Namespace: "kernel-control", Image: "ghcr.io/x/gentian-os:develop-abc", Seeder: seeder}
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	held := func() (string, string) {
		db, _ := vault.Get(ctx, "gentian-os/kernel/database/openfga-apps")
		k, _ := vault.Get(ctx, "gentian-os/kernel/authz/openfga-apps")
		return db["password"], k["preshared_key"]
	}
	if pw, key := held(); pw != "" || key != "" {
		t.Fatal("a purged engine got credentials")
	}

	set.Data[EngineAuthorization] = engineWanted
	if err := c.Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	// Both credentials are the vault's, made once, and reach the instance
	// and the arbiter by ExternalSecrets of the kernel's store.
	pw, key := held()
	if len(pw) < 30 || len(key) < 30 {
		t.Fatalf("vault = %q %q", pw, key)
	}
	for name, want := range map[string]string{
		arbiter.DatastoreSecret: "gentian-os/kernel/database/openfga-apps",
		arbiter.KeySecret:       "gentian-os/kernel/authz/openfga-apps",
	} {
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(externalSecretGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: arbiter.Namespace}, es); err != nil {
			t.Fatal(err)
		}
		data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
		store, _, _ := unstructured.NestedString(es.Object, "spec", "secretStoreRef", "name")
		if len(data) == 0 || data[0].(map[string]any)["remoteRef"].(map[string]any)["key"] != want || store != secrets.KernelStore {
			t.Fatalf("%s: %v %s", name, data, store)
		}
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
	got := dep.Spec.Template.Spec.Containers[0]
	if got.Image != r.Image || got.Command[0] != "/arbiter" {
		t.Fatalf("container = %s %v", got.Image, got.Command)
	}
	keyFromSecret := false
	for _, env := range got.Env {
		if env.Name == "OPENFGA_API_TOKEN" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef.Name == arbiter.KeySecret {
			keyFromSecret = true
		}
	}
	if !keyFromSecret {
		t.Fatal("the arbiter does not read the instance's key from the delivered Secret")
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
		for _, res := range rule.Resources {
			if res == "secrets" {
				t.Fatal("the arbiter may touch Secrets")
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
	// The credentials are kept.
	if _, err := r.Reconcile(ctx, authorizationEngineRequest); err != nil {
		t.Fatal(err)
	}
	if pw2, key2 := held(); pw2 != pw || key2 != key {
		t.Fatal("the vault's credentials changed")
	}
}
