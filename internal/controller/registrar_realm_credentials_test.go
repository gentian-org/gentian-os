/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/controller"
)

// The hand-over Secret, and the one piece of it that is not obvious.
//
// A realm this pass could not reach is not the same as a realm that no longer
// exists. Replacing the Secret on a partial pass would withdraw a working
// credential over a transient 404 while a realm is still composing, and the
// People screen would go to 503 for as long as the outage lasted. A complete
// pass replaces, so a realm that IS gone stops being reachable.

func TestRegistrarRealmSecretReplacesOnACompletePass(t *testing.T) {
	t.Parallel()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.RegistrarRealmSecretName,
			Namespace: controller.RegistrarRealmSecretNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("old"), "retired": []byte("gone")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()

	err := controller.WriteRegistrarRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("new"), "demo": []byte("d")}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: controller.RegistrarRealmSecretNamespaceForTest()}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["kernel"]) != "new" || string(got.Data["demo"]) != "d" {
		t.Fatalf("data = %v", got.Data)
	}
	if _, still := got.Data["retired"]; still {
		t.Error("a realm that is gone must stop being reachable")
	}
}

func TestRegistrarRealmSecretMergesOnAPartialPass(t *testing.T) {
	t.Parallel()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.RegistrarRealmSecretName,
			Namespace: controller.RegistrarRealmSecretNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("k"), "demo": []byte("d")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()

	// demo could not be reached this pass; kernel was.
	err := controller.WriteRegistrarRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k2")}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: controller.RegistrarRealmSecretNamespaceForTest()}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["kernel"]) != "k2" {
		t.Errorf("the realm that WAS reached was not updated: %v", got.Data)
	}
	if string(got.Data["demo"]) != "d" {
		t.Error("a realm that could not be reached this pass lost its working credential")
	}
}

func TestRegistrarRealmSecretIsCreatedWhenAbsent(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).Build()

	if err := controller.WriteRegistrarRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k")}, true); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: controller.RegistrarRealmSecretNamespaceForTest()}, got); err != nil {
		t.Fatalf("the Secret was not created: %v", err)
	}
	if string(got.Data["kernel"]) != "k" {
		t.Fatalf("data = %v", got.Data)
	}
}

// An unchanged pass must not write. A Secret rewritten every five minutes has
// a resourceVersion that changes constantly, which makes a real change
// impossible to pick out in an audit.
func TestAnUnchangedPassDoesNotWrite(t *testing.T) {
	t.Parallel()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.RegistrarRealmSecretName,
			Namespace: controller.RegistrarRealmSecretNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("k")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()
	before := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: controller.RegistrarRealmSecretNamespaceForTest()}, before); err != nil {
		t.Fatal(err)
	}

	if err := controller.WriteRegistrarRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k")}, true); err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: controller.RegistrarRealmSecretNamespaceForTest()}, after); err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("an unchanged pass wrote: %s → %s", before.ResourceVersion, after.ResourceVersion)
	}
}

// The Secret the director mounted is removed, the registrar's is left alone,
// and a cluster that never had the old one is not an error.
func TestTheDirectorsFormerSecretIsRetired(t *testing.T) {
	t.Parallel()
	ns := controller.RegistrarRealmSecretNamespaceForTest()
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gentian-director-realms", Namespace: ns},
		Data:       map[string][]byte{"kernel": []byte("former")},
	}
	current := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: controller.RegistrarRealmSecretName, Namespace: ns},
		Data:       map[string][]byte{"kernel": []byte("k")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(old, current).Build()

	for pass := 1; pass <= 2; pass++ {
		if err := controller.RetireDirectorRealmSecretForTest(context.Background(), c); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	err := c.Get(context.Background(), types.NamespacedName{Name: old.Name, Namespace: ns}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the director's former Secret is still there: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.RegistrarRealmSecretName, Namespace: ns}, &corev1.Secret{}); err != nil {
		t.Fatalf("the registrar's Secret was removed: %v", err)
	}
}
