/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func TestDirectorRealmSecretReplacesOnACompletePass(t *testing.T) {
	t.Parallel()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.DirectorRealmSecretName,
			Namespace: controller.ServicesNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("old"), "retired": []byte("gone")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()

	err := controller.WriteDirectorRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("new"), "demo": []byte("d")}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.DirectorRealmSecretName, Namespace: controller.ServicesNamespaceForTest()}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["kernel"]) != "new" || string(got.Data["demo"]) != "d" {
		t.Fatalf("data = %v", got.Data)
	}
	if _, still := got.Data["retired"]; still {
		t.Error("a realm that is gone must stop being reachable")
	}
}

func TestDirectorRealmSecretMergesOnAPartialPass(t *testing.T) {
	t.Parallel()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.DirectorRealmSecretName,
			Namespace: controller.ServicesNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("k"), "demo": []byte("d")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()

	// demo could not be reached this pass; kernel was.
	err := controller.WriteDirectorRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k2")}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.DirectorRealmSecretName, Namespace: controller.ServicesNamespaceForTest()}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["kernel"]) != "k2" {
		t.Errorf("the realm that WAS reached was not updated: %v", got.Data)
	}
	if string(got.Data["demo"]) != "d" {
		t.Error("a realm that could not be reached this pass lost its working credential")
	}
}

func TestDirectorRealmSecretIsCreatedWhenAbsent(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).Build()

	if err := controller.WriteDirectorRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k")}, true); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.DirectorRealmSecretName, Namespace: controller.ServicesNamespaceForTest()}, got); err != nil {
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
			Name:      controller.DirectorRealmSecretName,
			Namespace: controller.ServicesNamespaceForTest(),
		},
		Data: map[string][]byte{"kernel": []byte("k")},
	}
	c := fake.NewClientBuilder().WithScheme(controller.SchemeForTest(t)).WithObjects(existing).Build()
	before := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.DirectorRealmSecretName, Namespace: controller.ServicesNamespaceForTest()}, before); err != nil {
		t.Fatal(err)
	}

	if err := controller.WriteDirectorRealmSecretForTest(context.Background(), c,
		map[string][]byte{"kernel": []byte("k")}, true); err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: controller.DirectorRealmSecretName, Namespace: controller.ServicesNamespaceForTest()}, after); err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("an unchanged pass wrote: %s → %s", before.ResourceVersion, after.ResourceVersion)
	}
}
