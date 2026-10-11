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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gentian-org/gentian-os/internal/bouncer"
)

func exchangeSecret(name, realm, value string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: servicesNamespace}, Data: map[string][]byte{}}
	if realm != "" {
		s.Labels = map[string]string{exchangeRealmLabel: realm}
	}
	if value != "" {
		s.Data[exchangeSecretKey] = []byte(value)
	}
	return s
}

// The bouncer mounts one Secret with an entry per realm, gathered from the
// Secrets the tenants' realms are given; a realm whose name could not be a
// file name, or whose secret is not there yet, has no entry.
func TestExchangeSecretsAreGatheredByRealm(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	elsewhere := exchangeSecret("acme-exchange", "acme", "stolen")
	elsewhere.Namespace = "tenant-acme"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		exchangeSecret("acme-exchange", "acme", "acme-secret"),
		exchangeSecret("globex-exchange", "globex", "globex-secret"),
		exchangeSecret("empty-exchange", "empty", ""),
		exchangeSecret("odd-exchange", "../acme", "odd"),
		exchangeSecret("unlabelled", "", "nobody's"),
		// The same label outside the edge namespace is not the edge's.
		elsewhere,
	).Build()
	r := &GatewayPlatformReconciler{Client: c}
	read := func() map[string][]byte {
		s := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: bouncerExchangeSecret, Namespace: servicesNamespace}, s); err != nil {
			t.Fatal(err)
		}
		return s.Data
	}
	if err := r.ensureBouncerExchangeSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	got := read()
	if len(got) != 2 || string(got["acme"]) != "acme-secret" || string(got["globex"]) != "globex-secret" {
		t.Fatalf("gathered %v", got)
	}

	// A realm goes: its entry goes.
	if err := c.Delete(ctx, exchangeSecret("globex-exchange", "globex", "")); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureBouncerExchangeSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if got = read(); len(got) != 1 || string(got["acme"]) != "acme-secret" {
		t.Fatalf("after a realm went: %v", got)
	}
}

// A Secret of the bouncer's name that the operator did not write is not
// filled with every realm's secret.
func TestExchangeSecretsAreNotWrittenIntoAForeignSecret(t *testing.T) {
	scheme := rightsScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		exchangeSecret("acme-exchange", "acme", "acme-secret"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bouncerExchangeSecret, Namespace: servicesNamespace}},
	).Build()
	r := &GatewayPlatformReconciler{Client: c}
	if err := r.ensureBouncerExchangeSecrets(context.Background()); err == nil {
		t.Fatal("a foreign Secret was filled")
	}
}

func exchangeRoute(name, host string, annotations map[string]string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "tenant-acme",
		Labels:      map[string]string{bouncerRouteLabel: "true"},
		Annotations: map[string]string{bouncerRelationAnnotation: "can_use", bouncerObjectAnnotation: "app:acme/wiki", bouncerAuthModeAnnotation: "oidc"},
	}}
	for k, v := range annotations {
		route.Annotations[k] = v
	}
	route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(host)}
	return route
}

// A route that asks for the app's token says so in the table, a host is
// handed one token or the other, and the bouncer reads what was written.
func TestTheTableSaysWhichHostsExchange(t *testing.T) {
	scheme := rightsScheme(t)
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	scope := AppTokenScope("wiki")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Two entries of one component: the host exchanges if either asks.
		exchangeRoute("wiki-web", "wiki.acme.example", nil),
		exchangeRoute("wiki-api", "wiki.acme.example", map[string]string{bouncerExchangeScopeAnnotation: scope}),
		exchangeRoute("plain", "plain.acme.example", nil),
		// Should a host ever say both, the session's token is not also exchanged.
		exchangeRoute("both", "both.acme.example", map[string]string{bouncerExchangeScopeAnnotation: scope, bouncerForwardAnnotation: "true"}),
	).Build()
	entries, err := componentRouteTableEntries(context.Background(), client.Reader(c))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := bouncerTable(nil, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	table, err := bouncer.ParseTable([]byte(rendered))
	if err != nil {
		t.Fatalf("the bouncer refuses the operator's table: %v", err)
	}
	if got := table.Match("wiki.acme.example"); got == nil || got.ExchangeScope != "app-wiki" {
		t.Fatalf("wiki: %+v", got)
	}
	if got := table.Match("plain.acme.example"); got == nil || got.ExchangeScope != "" {
		t.Fatalf("plain: %+v", got)
	}
	if got := table.Match("both.acme.example"); got == nil || got.ExchangeScope != "" || !got.ForwardToken {
		t.Fatalf("both: %+v", got)
	}
}
