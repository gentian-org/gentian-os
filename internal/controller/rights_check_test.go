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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
)

func rightsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func rightsProfile(name string, wants bool) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if wants {
		profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
			Services: &gentianov1alpha1.ServiceRequirements{Rights: &gentianov1alpha1.RightsRequirement{}},
		}
	}
	return profile
}

func rightsComponent(tenant, name string) *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "tenant-" + tenant, UID: types.UID(tenant + "-" + name),
	}}
	comp.Spec.ProfileRef.Name = name
	return comp
}

func rightsSecret(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: rightsCheckSecretName(comp.Name), Namespace: comp.Namespace}
	if err := c.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the component's secret: %v", err)
	}
	return secret
}

// A component that declares the requirement is given a key once, and keeps
// it. One that does not declare it is given nothing.
func TestRightsCheckKeyIsMadeOnceAndKept(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	comp, plain := rightsComponent("acme", "notary"), rightsComponent("acme", "plain")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, plain).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if err := r.ensureRightsCheck(ctx, comp, rightsProfile("notary", true)); err != nil {
		t.Fatal(err)
	}
	first := rightsSecret(t, c, comp)
	key := string(first.Data[rightsCheckKeyKey])
	if len(key) < 40 {
		t.Fatalf("key %q is too short to be random", key)
	}
	if url := string(first.Data[rightsCheckURLKey]); !strings.HasSuffix(url, ":8082/v1/check") {
		t.Fatalf("address %q does not name the bouncer's listener for the question", url)
	}
	if first.Labels[rightsCheckerLabel] != "true" || !metav1.IsControlledBy(first, comp) {
		t.Fatalf("labels %v, owners %v", first.Labels, first.OwnerReferences)
	}

	if err := r.ensureRightsCheck(ctx, comp, rightsProfile("notary", true)); err != nil {
		t.Fatal(err)
	}
	if again := string(rightsSecret(t, c, comp).Data[rightsCheckKeyKey]); again != key {
		t.Fatal("the key changed on a second reconcile")
	}

	if err := r.ensureRightsCheck(ctx, plain, rightsProfile("plain", false)); err != nil {
		t.Fatal(err)
	}
	name := types.NamespacedName{Name: rightsCheckSecretName("plain"), Namespace: plain.Namespace}
	if err := c.Get(ctx, name, &corev1.Secret{}); err == nil {
		t.Fatal("a component that declared nothing was given a key")
	}
}

// A Secret of that name which is somebody else's is not adopted.
func TestRightsCheckDoesNotAdoptAForeignSecret(t *testing.T) {
	scheme := rightsScheme(t)
	comp := rightsComponent("acme", "notary")
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rightsCheckSecretName("notary"), Namespace: comp.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, foreign).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureRightsCheck(context.Background(), comp, rightsProfile("notary", true)); err == nil {
		t.Fatal("a foreign Secret was adopted")
	}
}

// The table holds a checker for a component's own key and for nothing a
// label alone claims; and the bouncer reads what the operator wrote.
func TestBouncerTableHoldsOnlyCheckersTheOperatorMade(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	notary, plain := rightsComponent("acme", "notary"), rightsComponent("acme", "plain")
	objects := []client.Object{notary, plain, rightsProfile("notary", true), rightsProfile("plain", false)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureRightsCheck(ctx, notary, rightsProfile("notary", true)); err != nil {
		t.Fatal(err)
	}
	key := string(rightsSecret(t, c, notary).Data[rightsCheckKeyKey])

	labelled := func(namespace, name string, owner *gentianov1alpha1.Component) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{rightsCheckerLabel: "true"}},
			Data:       map[string][]byte{rightsCheckKeyKey: []byte("forged-" + namespace + name)},
		}
		if owner != nil {
			yes := true
			s.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: gentianov1alpha1.GroupVersion.String(), Kind: "Component",
				Name: owner.Name, UID: owner.UID, Controller: &yes,
			}}
		}
		return s
	}
	for _, forged := range []*corev1.Secret{
		// A label and nothing else.
		labelled("tenant-acme", "rights-check-loose", nil),
		// Outside any tenant's namespace.
		labelled("kernel-edge", "rights-check-notary", notary),
		// Owned by a component whose profile does not declare the requirement.
		labelled("tenant-acme", "rights-check-plain", plain),
		// Owned by a component that is not there.
		labelled("tenant-globex", "rights-check-notary", rightsComponent("globex", "notary")),
	} {
		if err := c.Create(ctx, forged); err != nil {
			t.Fatal(err)
		}
	}

	checkers, err := rightsCheckers(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if len(checkers) != 1 || checkers[0] != (bouncerChecker{Tenant: "acme", Component: "notary", KeyHash: hex.EncodeToString(sum[:])}) {
		t.Fatalf("checkers = %+v, want the notary of acme alone", checkers)
	}

	// The profile stops declaring the requirement: the key is worth nothing
	// from the next table on, though the Secret is still there.
	stopped := rightsProfile("notary", false)
	current := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stopped), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Requires = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if after, err := rightsCheckers(ctx, c); err != nil || len(after) != 0 {
		t.Fatalf("after the profile stopped declaring it: %+v, %v", after, err)
	}

	rendered, err := bouncerTable(nil, nil, checkers)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, key) {
		t.Fatal("the table carries the key itself")
	}
	table, err := bouncer.ParseTable([]byte(rendered))
	if err != nil {
		t.Fatalf("the bouncer refuses the operator's table: %v", err)
	}
	if len(table.Checkers) != 1 || table.Checkers[0].Tenant != "acme" {
		t.Fatalf("the bouncer read %+v", table.Checkers)
	}
	if without, _ := bouncerTable(nil, nil, nil); strings.Contains(without, "checkers") {
		t.Fatal("a table with no checker mentions them")
	}
}

func TestRightsCheckOpensOnePortOfTheEdge(t *testing.T) {
	rule := rightsCheckEgress()
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != servicesNamespace {
		t.Fatalf("peer = %+v", rule.To)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != rightsCheckPort {
		t.Fatalf("ports = %+v, want the one listener", rule.Ports)
	}
}
