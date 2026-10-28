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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// A base whose addon declares TURN is given the edge's address and the
// cluster's secret, the same secret the edge holds; once nothing it runs
// declares TURN, the Secret goes.
func TestTURNCredentialsFollowWhatTheComponentRuns(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	vault := memVault{}
	seeder := secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))
	r := &ComponentReconciler{Client: c, Scheme: s, KernelRealm: "kernel", KernelDomain: "k.example", Seeder: seeder}
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce", Namespace: "tenant-acme", UID: "uid-nc"}}
	base := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce"}}
	talk := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-spreed-ce"}}
	talk.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{TURN: &gentianov1alpha1.TURNRequirement{}}}

	if err := r.ensureTURNCredentials(ctx, comp, base, []*gentianov1alpha1.ComponentProfile{talk}); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: "turn-credentials-nextcloud-base-ce", Namespace: "tenant-acme"}
	got := &corev1.Secret{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	edge, err := turnSharedSecret(ctx, seeder)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		turnServerKey: "turn.k.example",
		turnPortKey:   "3478",
		turnsPortKey:  "443",
		turnURIsKey:   "turn:turn.k.example:3478?transport=udp,turn:turn.k.example:3478?transport=tcp,turns:turn.k.example:443?transport=tcp",
		stunURIKey:    "stun:turn.k.example:3478",
		turnSecretKey: edge,
	}
	for k, v := range want {
		if string(got.Data[k]) != v {
			t.Errorf("%s = %q, want %q", k, got.Data[k], v)
		}
	}
	if !metav1.IsControlledBy(got, comp) {
		t.Fatal("the Secret is not the component's")
	}

	// The addon's own Component deploys nothing and is given nothing.
	talk.Spec.Package.Addon = &gentianov1alpha1.PackageAddon{ID: "spreed", Of: "nextcloud-base-ce"}
	addonComp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-spreed-ce", Namespace: "tenant-acme", UID: "uid-talk"}}
	if err := r.ensureTURNCredentials(ctx, addonComp, talk, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "turn-credentials-nextcloud-spreed-ce", Namespace: "tenant-acme"}, &corev1.Secret{}); !errors.IsNotFound(err) {
		t.Fatalf("the addon's own Component was given the secret: %v", err)
	}

	// Talk switched off: nothing the component runs declares TURN.
	if err := r.ensureTURNCredentials(ctx, comp, base, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &corev1.Secret{}); !errors.IsNotFound(err) {
		t.Fatalf("the Secret outlived the requirement: %v", err)
	}
}
