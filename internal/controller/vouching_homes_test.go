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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The home scopes are written into a vouching Secret nobody else keeps, and
// left out of one External Secrets owns: it would write the Secret back and
// make a new client secret each time.
func TestTheHomeScopesStayOutOfASecretExternalSecretsOwns(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	plain := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: VouchingSecretName("editor"), Namespace: "tenant-acme"},
		Data: map[string][]byte{VouchingClientIDKey: []byte("vouch-editor")}}
	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: VouchingSecretName("porter"), Namespace: "tenant-acme",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret",
			Name: VouchingSecretName("porter"), UID: "u1", Controller: ptr(true)}}},
		Data: map[string][]byte{VouchingClientIDKey: []byte("vouch-porter")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(plain, owned).Build()
	r := &TileProjectionReconciler{Client: c}
	ctx := context.Background()
	homes := map[string]string{"file-store": "nextcloud"}
	for _, p := range []string{"editor", "porter"} {
		if err := r.keepVouchingHomes(ctx, "tenant-acme", p, homes); err != nil {
			t.Fatal(err)
		}
	}
	got := func(p string) map[string][]byte {
		s := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: VouchingSecretName(p)}, s); err != nil {
			t.Fatal(err)
		}
		return s.Data
	}
	if string(got("editor")[VouchingHomeKey("file-store")]) != "home-nextcloud" {
		t.Fatalf("the plain Secret: %v", got("editor"))
	}
	if d := got("porter"); len(d) != 1 {
		t.Fatalf("the Secret External Secrets owns was written: %v", d)
	}
}
