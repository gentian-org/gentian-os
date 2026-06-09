// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestReconcileKeycloakIDPEmbeddingIngressPatchesIngress(t *testing.T) {
	t.Setenv("KEYCLOAK_PROXY_INGRESS_NAME", "id-proxy")
	t.Setenv("SERVICES_NAMESPACE", "gentian-dev")

	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Spec.Domain = "demo.desk.gentian.org"

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "id-proxy",
			Namespace: "gentian-dev",
			Annotations: map[string]string{
				nginxConfigurationSnippetAnnotation: "stale",
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = networkingv1.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, ing).Build()

	if err := reconcileKeycloakIDPEmbeddingIngress(context.Background(), c, "desk.gentian.org", gentianov1alpha1.TenancyModeMulti); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "id-proxy", Namespace: "gentian-dev"}, got); err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	want := keycloakOIDCEmbeddingIngressSnippet("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	if got.Annotations[nginxConfigurationSnippetAnnotation] != want {
		t.Fatalf("snippet mismatch:\nwant:\n%s\ngot:\n%s", want, got.Annotations[nginxConfigurationSnippetAnnotation])
	}
}
