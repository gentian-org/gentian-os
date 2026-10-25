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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

const ledgerModel = `model
  schema 1.1
type user
type ledger
  relations
    define owner: [user]
    define viewer: [user] or owner
`

func authorizationProfile(name, model string) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if model != "" {
		profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
			Services: &gentianov1alpha1.ServiceRequirements{
				Authorization: &gentianov1alpha1.AuthorizationRequirement{Model: model},
			},
		}
	}
	return profile
}

func authorizationSecret(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: authorizationSecretName(comp.Name), Namespace: comp.Namespace}
	if err := c.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the component's secret: %v", err)
	}
	return secret
}

// A component that declares a store is given a key once, and keeps it. One
// that does not declare it is given nothing.
func TestAuthorizationKeyIsMadeOnceAndKept(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	comp, plain := rightsComponent("acme", "ledger"), rightsComponent("acme", "plain")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, plain).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if err := r.ensureAuthorization(ctx, comp, authorizationProfile("ledger", ledgerModel)); err != nil {
		t.Fatal(err)
	}
	first := authorizationSecret(t, c, comp)
	key := string(first.Data[authorizationKeyKey])
	if len(key) < 40 {
		t.Fatalf("key %q is too short to be random", key)
	}
	if url := string(first.Data[authorizationURLKey]); url != "http://arbiter.system-authorization.svc:8080" {
		t.Fatalf("address %q is not the arbiter's", url)
	}
	if first.Labels[authorizationHolderLabel] != "true" || !metav1.IsControlledBy(first, comp) {
		t.Fatalf("labels %v, owners %v", first.Labels, first.OwnerReferences)
	}
	if err := r.ensureAuthorization(ctx, comp, authorizationProfile("ledger", ledgerModel)); err != nil {
		t.Fatal(err)
	}
	if again := string(authorizationSecret(t, c, comp).Data[authorizationKeyKey]); again != key {
		t.Fatal("the key changed on a second reconcile")
	}

	if err := r.ensureAuthorization(ctx, plain, authorizationProfile("plain", "")); err != nil {
		t.Fatal(err)
	}
	name := types.NamespacedName{Name: authorizationSecretName("plain"), Namespace: plain.Namespace}
	if err := c.Get(ctx, name, &corev1.Secret{}); err == nil {
		t.Fatal("a component that declared nothing was given a key")
	}
}

// A Secret of that name which is somebody else's is not adopted.
func TestAuthorizationDoesNotAdoptAForeignSecret(t *testing.T) {
	scheme := rightsScheme(t)
	comp := rightsComponent("acme", "ledger")
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authorizationSecretName("ledger"), Namespace: comp.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, foreign).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAuthorization(context.Background(), comp, authorizationProfile("ledger", ledgerModel)); err == nil {
		t.Fatal("a foreign Secret was adopted")
	}
}

// The table holds the key's hash, never the key, with the compiled model --
// for a component that declares a store now, in a tenant's namespace, under
// a Secret it controls.
func TestTheArbitersTableHoldsTheKeysHashAndTheCompiledModel(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	ledger, gone := rightsComponent("acme", "ledger"), rightsComponent("acme", "gone")
	gone.Spec.ProfileRef.Name = "plain"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ledger, gone, authorizationProfile("ledger", ledgerModel), authorizationProfile("plain", "")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAuthorization(ctx, ledger, authorizationProfile("ledger", ledgerModel)); err != nil {
		t.Fatal(err)
	}
	// "gone" held a key when its profile declared a store, and declares none now.
	if err := r.ensureAuthorization(ctx, gone, authorizationProfile("gone", ledgerModel)); err != nil {
		t.Fatal(err)
	}
	// A Secret outside a tenant's namespace carrying the label is ignored.
	stray := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "authorization-x", Namespace: "kernel-control", Labels: map[string]string{authorizationHolderLabel: "true"},
	}, Data: map[string][]byte{authorizationKeyKey: []byte("k")}}
	if err := c.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}

	stores, err := authorizationStores(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 1 || stores[0].Tenant != "acme" || stores[0].Component != "ledger" {
		t.Fatalf("stores = %+v", stores)
	}
	key := authorizationSecret(t, c, ledger).Data[authorizationKeyKey]
	sum := sha256.Sum256(key)
	if stores[0].KeyHash != hex.EncodeToString(sum[:]) || strings.Contains(string(stores[0].Model), string(key)) {
		t.Fatalf("entry = %+v", stores[0])
	}
	want, _ := arbiter.Compile(ledgerModel)
	if string(stores[0].Model) != string(want) {
		t.Fatalf("model = %s, want %s", stores[0].Model, want)
	}
}

// A model that does not compile is refused before anything is written.
func TestAnAuthorizationModelThatDoesNotCompileHoldsTheInstall(t *testing.T) {
	if refusal := authorizationModelRefusal(authorizationProfile("ledger", "model\n  schema 1.1\ntype x\n  relations\n    define\n")); refusal == "" {
		t.Fatal("a broken model was not refused")
	}
	if refusal := authorizationModelRefusal(authorizationProfile("ledger", ledgerModel)); refusal != "" {
		t.Fatalf("a good model was refused: %s", refusal)
	}
	if refusal := authorizationModelRefusal(authorizationProfile("plain", "")); refusal != "" {
		t.Fatalf("a profile without a store was refused: %s", refusal)
	}
}

// The way to the arbiter is its pods on its port, in its namespace, and the
// operator, the arbiter and the tenant's policy agree on the port and labels.
func TestTheWayToTheArbiterIsItsPodsOnItsPort(t *testing.T) {
	rule := authorizationEgress()
	if len(rule.To) != 1 || rule.To[0].PodSelector == nil || rule.To[0].NamespaceSelector == nil {
		t.Fatalf("rule = %+v", rule)
	}
	if rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != arbiter.Namespace {
		t.Fatalf("namespace = %v", rule.To[0].NamespaceSelector.MatchLabels)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != arbiter.Port || netpolicy.ArbiterPort != arbiter.Port {
		t.Fatalf("ports = %+v", rule.Ports)
	}
	if rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != arbiter.ServiceName {
		t.Fatalf("pods = %v", rule.To[0].PodSelector.MatchLabels)
	}
}
