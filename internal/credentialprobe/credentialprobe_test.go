/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package credentialprobe

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// probe is a credential requirement's probe as ESO leaves it: Ready True
// once every field is in the vault.
func probe(requirement string, ready bool) client.Object {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	es.SetNamespace(meta.OperatorNamespace)
	es.SetName("credreq-" + requirement)
	status := "False"
	if ready {
		status = "True"
	}
	_ = unstructured.SetNestedSlice(es.Object, []any{map[string]any{"type": "Ready", "status": status}}, "status", "conditions")
	return es
}

// The credentials a person supplies that are not set are the ones the app
// waits for, named as the Admin Console shows them; a set one is not named,
// and one whose probe cannot be found counts as not set.
func TestTheSuppliedCredentialsNotSetAreTheOnesAwaited(t *testing.T) {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{
		{Name: "payments", DisplayName: "Payment keys"},
		{Name: "mailer", DisplayName: "Mail API token"},
		{Name: "maps"},
	}}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(
		probe("app-demo-shop-payments", false),
		probe("app-demo-shop-mailer", true),
	).Build()

	got := AwaitedSupplied(context.Background(), c, "demo", "shop", profile)
	if len(got) != 2 || got[0] != (Awaited{Name: "payments", DisplayName: "Payment keys", Requirement: "app-demo-shop-payments"}) ||
		got[1].Name != "maps" {
		t.Fatalf("awaited = %+v", got)
	}
	text := Text(got)
	for _, want := range []string{"Payment keys (payments)", "maps (maps)", "under Credentials", "no bundle carries them"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q does not say %q", text, want)
		}
	}
	if AwaitedSupplied(context.Background(), c, "demo", "shop", nil) != nil || Text(nil) != "" {
		t.Error("an app without a profile is said to wait")
	}
}
