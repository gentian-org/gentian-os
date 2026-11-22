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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// A grant has its effect when it is made, not at the next hourly refresh:
// the consumer's values Secret is read again until it holds the address, and
// its release is then made to take it. Each is asked once, not on every pass,
// and a consumer that maps no address for the contract is left alone.
func TestAGrantedAddressIsSeenThroughToTheConsumer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	const ns, address = "tenant-demo", "http://provider-api.tenant-demo.svc.cluster.local:8000/v1"
	path := secrets.ContractPath("demo", "orders")

	binding := func(consumer string) *gentianov1alpha1.IntegrationBinding {
		return &gentianov1alpha1.IntegrationBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "demo--" + consumer + "--orders", Namespace: ns,
				Labels: map[string]string{tenantLabel: "demo"},
			},
			Spec: gentianov1alpha1.IntegrationBindingSpec{
				Contract: "orders",
				Provider: gentianov1alpha1.AppEndpoint{App: "provider", Namespace: ns},
				Consumer: gentianov1alpha1.AppEndpoint{App: consumer, Namespace: ns},
				Auth:     &gentianov1alpha1.BindingAuth{VaultPath: path},
				Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "provider-api", Port: 8000, Path: "/v1"},
			},
		}
	}
	grant := func(consumer string) *gentianov1alpha1.AppGrant {
		return &gentianov1alpha1.AppGrant{
			ObjectMeta: metav1.ObjectMeta{Name: consumer, Namespace: ns},
			Spec: gentianov1alpha1.AppGrantSpec{
				App:     consumer,
				Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "orders", Granted: []string{"read"}}},
			},
		}
	}
	externalSecret := func(consumer string, keys ...string) *unstructured.Unstructured {
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(secrets.ExternalSecretGVK)
		es.SetName(consumer + "-sensitive-values")
		es.SetNamespace(ns)
		data := make([]any, 0, len(keys))
		for _, k := range keys {
			data = append(data, map[string]any{"secretKey": k})
		}
		_ = unstructured.SetNestedSlice(es.Object, data, "spec", "data")
		return es
	}
	values := func(consumer string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-sensitive-values", Namespace: ns},
			Data:       map[string][]byte{"integration-orders-endpoint": nil},
		}
	}

	b, other := binding("consumer"), binding("other")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(b, other, grant("consumer"), grant("other"),
			externalSecret("consumer", "db-password", "integration-orders-endpoint"), values("consumer"),
			directRelease(ns+"-consumer", nil),
			// Maps the contract's password and no address.
			externalSecret("other", "integration-orders-password"), values("other"),
			directRelease(ns+"-other", nil)).
		WithStatusSubresource(b, other).Build()
	now := time.Unix(1_700_000_000, 0)
	r := &IntegrationBindingReconciler{
		Client: c, Scheme: scheme,
		Seeder: secrets.NewSeeder(memVault{path: {"password": "shared"}}, nil),
		Now:    func() time.Time { return now },
	}
	pass := func(ib *gentianov1alpha1.IntegrationBinding) ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ib)})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		return res
	}
	read := func(kind *unstructured.Unstructured, name, namespace string) *unstructured.Unstructured {
		t.Helper()
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(kind.GroupVersionKind())
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	es := func(consumer string) *unstructured.Unstructured {
		return read(externalSecret(consumer), consumer+"-sensitive-values", ns)
	}
	release := func(consumer string) *unstructured.Unstructured {
		return read(directRelease("", nil), ns+"-"+consumer, "")
	}
	const mark = addressDeliveredPrefix + "orders"

	// The Secret holds no address yet: it is read again, and looked at again.
	if res := pass(b); res.RequeueAfter == 0 {
		t.Fatal("the reconciler does not look again while the Secret lacks the address")
	}
	asked := es("consumer")
	if asked.GetAnnotations()[addressReadAnnotation] == "" {
		t.Fatal("the consumer's ExternalSecret was not read again at the grant")
	}
	if release("consumer").GetAnnotations()[mark] != "" {
		t.Fatal("the release was told to take an address its Secret does not hold yet")
	}

	// Asked a moment ago, it is not asked again; a minute on, it is.
	now = now.Add(20 * time.Second)
	if res := pass(b); res.RequeueAfter == 0 || es("consumer").GetResourceVersion() != asked.GetResourceVersion() {
		t.Fatal("the ExternalSecret was read again before the secrets operator had its minute")
	}
	now = now.Add(time.Minute)
	pass(b)
	if es("consumer").GetResourceVersion() == asked.GetResourceVersion() {
		t.Fatal("a read the secrets operator did not answer was never asked for again")
	}

	// The Secret arrives: the release is changed, once.
	secret := values("consumer")
	secret.Data["integration-orders-endpoint"] = []byte(address)
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if res := pass(b); res.RequeueAfter != 0 {
		t.Fatal("the reconciler keeps looking after the address was delivered")
	}
	told := release("consumer")
	if told.GetAnnotations()[mark] != address {
		t.Fatalf("the release was not made to take the address: %v", told.GetAnnotations())
	}
	pass(b)
	if release("consumer").GetResourceVersion() != told.GetResourceVersion() {
		t.Fatal("the release was changed again on a pass that had nothing to deliver")
	}

	// A consumer that maps no address is not read again and not changed.
	if res := pass(other); res.RequeueAfter != 0 {
		t.Fatal("the reconciler waits for an address the consumer does not map")
	}
	if es("other").GetAnnotations()[addressReadAnnotation] != "" || release("other").GetAnnotations()[mark] != "" {
		t.Fatal("a consumer that maps no address was touched")
	}
}
