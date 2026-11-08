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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A partner app's pods take its partners Secret as environment variables at
// start. The Secret arriving after them, or changing, has to roll them:
// the operator stamps its checksum on the app's pod templates.
func TestAPartnerAppRollsWhenItsSecretArrivesOrChanges(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	ns := "tenant-acme"
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "app-store", Namespace: ns}}
	comp.Spec.ProfileRef.Name = "app-store"
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "app-store"}}
	api := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "app-store-api", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/instance": "app-store"},
	}}
	other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, api, other).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	stampOf := func(name string) string {
		t.Helper()
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Annotations[partnersChecksumAnnotation]
	}

	// No Secret yet: nothing is stamped, nothing rolls.
	if err := r.rollOnPartnersSecret(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	if got := stampOf("app-store-api"); got != "" {
		t.Fatalf("stamped without a Secret: %q", got)
	}

	// The Secret arrives: the app's pods roll, no one else's.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PartnerSecretName("app-store"), Namespace: ns},
		Data:       map[string][]byte{"PARTNER_CLIENT_ID": []byte("partner-app-store")},
	}
	if err := c.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.rollOnPartnersSecret(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	first := stampOf("app-store-api")
	if first == "" || first != partnersChecksum(secret) {
		t.Fatalf("stamp = %q, want the Secret's checksum", first)
	}
	if stampOf("nextcloud") != "" {
		t.Fatal("another app's pods were rolled")
	}

	// Unchanged: the stamp stays. Changed: it moves.
	if err := r.rollOnPartnersSecret(ctx, comp, profile); err != nil || stampOf("app-store-api") != first {
		t.Fatalf("an unchanged Secret moved the stamp (%v)", err)
	}
	secret.Data["PARTNER_TOKEN_URL"] = []byte("http://keycloak/token")
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.rollOnPartnersSecret(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	if got := stampOf("app-store-api"); got == first || got != partnersChecksum(secret) {
		t.Fatalf("a changed Secret left the stamp at %q", got)
	}
}

// The Secret's events reach the Components of its profile, in its namespace.
func TestThePartnersSecretWakesItsProfilesComponents(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	mine := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "app-store", Namespace: "tenant-acme"}}
	mine.Spec.ProfileRef.Name = "app-store"
	elsewhere := mine.DeepCopy()
	elsewhere.Namespace = "tenant-beta"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mine, elsewhere).Build()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "partners-app-store", Namespace: "tenant-acme"}}
	reqs := mapPartnersSecret(context.Background(), c, secret)
	if len(reqs) != 1 || reqs[0].Namespace != "tenant-acme" || reqs[0].Name != "app-store" {
		t.Fatalf("requests = %v", reqs)
	}
	if got := mapPartnersSecret(context.Background(), c, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "tenant-acme"}}); len(got) != 0 {
		t.Fatalf("an unrelated Secret woke %v", got)
	}
}
