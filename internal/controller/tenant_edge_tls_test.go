/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The tenant certificate must cover exactly what the tenant listener can route.
//
// That listener carries hostname "*.<domain>" -- only one listener on :443 may
// leave its hostname unset, and the kernel listener is the one that does
// (4eb6235d). A listener hostname also gates route attachment, so a route for
// the bare apex can never attach there.
//
// Naming the apex in this certificate advertises a name the listener cannot
// route: the browser coalesces apex requests onto an open <sub>.<domain>
// connection under HTTP/2, Envoy selects the filter chain by SNI, and the
// request lands where no route matches -- 404 route_not_found, intermittent,
// on the portal the apex serves.
func TestBuildTenantWildcardCertificate_OmitsApex(t *testing.T) {
	t.Parallel()

	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	obj := buildTenantWildcardCertificate(
		tenant, "tenant-demo", "tenant-demo-wildcard",
		"tenant-demo-wildcard-tls", "demo.example.test", "issuer",
	)

	names, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
	if err != nil || !found {
		t.Fatalf("dnsNames not set: found=%v err=%v", found, err)
	}
	if len(names) != 1 || names[0] != "*.demo.example.test" {
		t.Fatalf("dnsNames = %v, want exactly [*.demo.example.test]", names)
	}
	for _, n := range names {
		if n == "demo.example.test" {
			t.Fatal("tenant certificate names the apex: the tenant listener is scoped to *.<domain> and cannot route it, so browsers coalesce apex requests onto a connection that answers 404")
		}
	}
}
