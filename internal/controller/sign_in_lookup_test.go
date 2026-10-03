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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The sign-in router finds a tenant on a custom domain by the domain's hash
// and nothing else: a tenant without one adds no entry, and the entry sends
// the browser to the console on the custom domain.
func TestTheSignInLookupHoldsOnlyCustomDomainsByTheirHash(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	acme := acmeTenantFixture()
	acme.Status.Domain = "acme.example"
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "plain"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(acme, plain).WithStatusSubresource(acme).Build()
	if err := c.Status().Update(ctx, acme); err != nil {
		t.Fatal(err)
	}
	r := &SignInLookupReconciler{Client: c, KernelDomain: "k.example", TenancyMode: "multi"}
	if _, err := r.Reconcile(ctx, signInLookupRequest); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, signInLookupRequest.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) != 1 {
		t.Fatalf("entries = %v, want the custom domain's only", cm.Data)
	}
	// sha256("acme.example"), as the page computes it with WebCrypto.
	want := "54667cc7be6265f6a4cdfe25b9c89d52aea7817c4e570cb678feec57c23f4a6a.json"
	got := signInLookupKey("acme.example")
	if got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if cm.Data[got] != `{"url":"https://console.acme.example/"}` {
		t.Fatalf("entry = %q", cm.Data[got])
	}
}
