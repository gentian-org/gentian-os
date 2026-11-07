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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)


func renderedKeycloakClient() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(keycloakClientGVK)
	obj.SetName("acme-mathesar-ce-keycloak-client")
	_ = unstructured.SetNestedStringMap(obj.Object, map[string]string{
		"name": "tenant-acme-mathesar-ce-oidc-client-creds", "namespace": "kernel-provisioning", "key": "client-secret",
	}, "spec", "forProvider", "clientSecretSecretRef")
	return obj
}

// A confidential client is not created before the Secret it reads its secret
// from has one: created earlier, the realm makes up a secret the app does not
// hold, and nobody can sign in to the app until the provider's next poll
// (#908). A client that exists is left to the provider.
func TestAClientIsNotCreatedBeforeItsSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(keycloakClientGVK, &unstructured.Unstructured{})
	listGVK := keycloakClientGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	ctx := context.Background()
	creds := func(value string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme-mathesar-ce-oidc-client-creds", Namespace: "kernel-provisioning"},
			Data:       map[string][]byte{"client-secret": []byte(value)},
		}
	}
	cases := []struct {
		name    string
		objects []client.Object
		held    bool
	}{
		{"no Secret yet", nil, true},
		{"a Secret without a value", []client.Object{creds("")}, true},
		{"the Secret with its value", []client.Object{creds("s3cret")}, false},
		{"the client exists", []client.Object{renderedKeycloakClient()}, false},
	}
	for _, c := range cases {
		r := &ComponentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objects...).Build()}
		waiting, err := r.secretRefPending(ctx, renderedKeycloakClient())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (waiting != "") != c.held {
			t.Errorf("%s: held=%t, want %t (%q)", c.name, waiting != "", c.held, waiting)
		}
	}

	// An object that reads no secret is never held.
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cm.SetName("x")
	r := &ComponentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if waiting, err := r.secretRefPending(ctx, cm); err != nil || waiting != "" {
		t.Errorf("a ConfigMap was held: %q %v", waiting, err)
	}
}
