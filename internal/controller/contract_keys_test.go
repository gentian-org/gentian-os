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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func contractBinding(consumer, provider, contract string) *gentianov1alpha1.IntegrationBinding {
	b := &gentianov1alpha1.IntegrationBinding{}
	b.Name = "acme--" + consumer + "--" + contract
	b.Namespace = "tenant-acme"
	b.Spec.Contract = contract
	b.Spec.Consumer.App, b.Spec.Provider.App = consumer, provider
	return b
}

func contractGrant(consumer string, contracts ...string) *gentianov1alpha1.AppGrant {
	g := &gentianov1alpha1.AppGrant{}
	g.Spec.App = consumer
	for _, c := range contracts {
		g.Spec.Consume = append(g.Spec.Consume, gentianov1alpha1.ConsumeGrantSpec{Contract: c, Granted: []string{"call"}})
	}
	return g
}

func secretOf(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "tenant-acme"}, s); err != nil {
		return nil
	}
	return s
}

// Each granted consumer holds a key of its own; the provider holds the
// hashes, by contract, and so knows which consumer is calling without
// holding anything it could call as one of them with. A grant that goes
// takes the consumer's key and its hash away.
func TestContractKeysFollowTheGrants(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "acme-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	bindings := []*gentianov1alpha1.IntegrationBinding{
		contractBinding("flow", "chaperone", "agent-tools"),
		contractBinding("other-engine", "chaperone", "agent-tools"),
		contractBinding("flow", "notary", "agent-notary"),
		contractBinding("ungranted", "chaperone", "agent-tools"),
	}
	grants := map[string]*gentianov1alpha1.AppGrant{
		"flow":         contractGrant("flow", "agent-tools", "agent-notary"),
		"other-engine": contractGrant("other-engine", "agent-tools"),
	}
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", bindings, grants); err != nil {
		t.Fatal(err)
	}

	flow := secretOf(t, c, contractKeySecretName("flow", "agent-tools"))
	other := secretOf(t, c, contractKeySecretName("other-engine", "agent-tools"))
	if flow == nil || other == nil || secretOf(t, c, contractKeySecretName("flow", "agent-notary")) == nil {
		t.Fatal("a granted consumer has no key")
	}
	flowKey, otherKey := string(flow.Data[contractKeyKey]), string(other.Data[contractKeyKey])
	if len(flowKey) < 40 || flowKey == otherKey {
		t.Fatalf("keys %q and %q: each consumer needs a random key of its own", flowKey, otherKey)
	}
	if string(flow.Data[contractProviderKey]) != "chaperone" || !metav1.IsControlledBy(flow, tenant) {
		t.Fatalf("the consumer's secret = %v, owners %v", flow.Data, flow.OwnerReferences)
	}
	if secretOf(t, c, contractKeySecretName("ungranted", "agent-tools")) != nil {
		t.Fatal("a consumer nobody granted was given a key")
	}

	callersOf := func() map[string]string {
		s := secretOf(t, c, contractCallersSecretName("chaperone"))
		if s == nil {
			return nil
		}
		out := map[string]string{}
		if err := json.Unmarshal(s.Data["agent-tools.json"], &out); err != nil {
			t.Fatal(err)
		}
		for _, v := range s.Data {
			if string(v) == flowKey || string(v) == otherKey {
				t.Fatal("the provider holds a consumer's key")
			}
		}
		return out
	}
	hash := func(k string) string { sum := sha256.Sum256([]byte(k)); return hex.EncodeToString(sum[:]) }
	callers := callersOf()
	if len(callers) != 2 || callers[hash(flowKey)] != "flow" || callers[hash(otherKey)] != "other-engine" {
		t.Fatalf("the chaperone's callers = %v", callers)
	}

	// A second reconcile changes nothing: the key is kept.
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", bindings, grants); err != nil {
		t.Fatal(err)
	}
	if again := string(secretOf(t, c, contractKeySecretName("flow", "agent-tools")).Data[contractKeyKey]); again != flowKey {
		t.Fatal("the key changed on a second reconcile")
	}

	// The other engine's grant is withdrawn.
	delete(grants, "other-engine")
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", bindings, grants); err != nil {
		t.Fatal(err)
	}
	if secretOf(t, c, contractKeySecretName("other-engine", "agent-tools")) != nil {
		t.Fatal("a consumer whose grant was withdrawn kept its key")
	}
	if callers = callersOf(); len(callers) != 1 || callers[hash(flowKey)] != "flow" {
		t.Fatalf("after the withdrawal the chaperone's callers = %v", callers)
	}

	// The last grants go: nothing is left.
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", bindings, nil); err != nil {
		t.Fatal(err)
	}
	left := &corev1.SecretList{}
	if err := c.List(ctx, left, client.InNamespace("tenant-acme")); err != nil || len(left.Items) != 0 {
		t.Fatalf("%d secrets left, %v", len(left.Items), err)
	}
}

// A Secret of the same name that is not this tenant's is neither adopted nor
// removed, and one that merely carries the label is left alone.
func TestContractKeysLeaveForeignSecretsAlone(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "acme-uid"}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: contractKeySecretName("flow", "agent-tools"), Namespace: "tenant-acme"}}
	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "somebody-elses", Namespace: "tenant-acme",
		Labels: map[string]string{contractSecretLabel: contractSecretKey, managedByLabel: managedByValue},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, foreign, labelled).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	bindings := []*gentianov1alpha1.IntegrationBinding{contractBinding("flow", "chaperone", "agent-tools")}
	grants := map[string]*gentianov1alpha1.AppGrant{"flow": contractGrant("flow", "agent-tools")}
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", bindings, grants); err == nil {
		t.Fatal("a foreign Secret was adopted")
	}
	if err := r.ensureContractKeys(ctx, tenant, "tenant-acme", nil, nil); err != nil {
		t.Fatal(err)
	}
	if secretOf(t, c, foreign.Name) == nil || secretOf(t, c, labelled.Name) == nil {
		t.Fatal("a Secret that is not this tenant's was removed")
	}
}
