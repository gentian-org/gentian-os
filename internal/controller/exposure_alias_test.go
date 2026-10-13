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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// aliasedShares is acme's published shares entry with an alias.
func aliasedShares(alias string, setAt time.Time) gentianov1alpha1.TenantExposure {
	at := metav1.NewTime(setAt)
	return gentianov1alpha1.TenantExposure{
		Install: "nextcloud-base-ce", ExposureName: "shares", Owner: "u-tom",
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
		Alias:     alias, AliasSetBy: "u-admin", AliasSetAt: &at,
	}
}

func perimeterListener(gw *gatewayv1.Gateway, host string) *gatewayv1.Listener {
	for i := range gw.Spec.Listeners {
		if l := &gw.Spec.Listeners[i]; l.Hostname != nil && string(*l.Hostname) == host {
			return l
		}
	}
	return nil
}

// A surface with an alias answers at <alias>.<cluster's domain> as well, on
// a listener of its own with the cluster's certificate, which names it.
func TestAnAliasGetsAListenerWithTheClustersCertificate(t *testing.T) {
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{aliasedShares("files", time.Now())}
	gw := buildPerimeterGateway("k.example", "multi", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())

	if perimeterListener(gw, "share.acme.k.example") == nil {
		t.Fatal("the surface lost the listener of its own address")
	}
	alias := perimeterListener(gw, "files.k.example")
	if alias == nil {
		t.Fatal("the alias has no listener")
	}
	if string(alias.Name) != perimeterListenerName("files.k.example") {
		t.Fatalf("the alias's listener is called %s; the route names it by its host", alias.Name)
	}
	ref := alias.TLS.CertificateRefs[0]
	if string(ref.Name) != kernelWildcardTLSSecretName || (ref.Namespace != nil && string(*ref.Namespace) != servicesNamespace) {
		t.Fatalf("the alias is served with %v, want the cluster's certificate", ref)
	}
}

// An alias that could not be honoured publishes nothing, whatever is in git:
// a name the platform keeps, a name a tenant is called, any alias on a
// single-tenancy cluster, and an alias on an entry that is not published.
func TestAnAliasThatIsNotHonouredGetsNoListener(t *testing.T) {
	other := &gentianov1alpha1.Tenant{}
	other.Name = "files"
	for name, c := range map[string]struct {
		mode    string
		alias   string
		entry   string
		tenants []gentianov1alpha1.Tenant
		host    string
	}{
		"a name the platform keeps":   {mode: "multi", alias: "id", entry: "shares", host: "id.k.example"},
		"the cluster's mail host":     {mode: "multi", alias: "mail", entry: "shares", host: "mail.k.example"},
		"a name a tenant is called":   {mode: "multi", alias: "files", entry: "shares", tenants: []gentianov1alpha1.Tenant{*other}, host: "files.k.example"},
		"a single-tenancy cluster":    {mode: "single", alias: "files", entry: "shares", host: "files.k.example"},
		"an entry that is not public": {mode: "multi", alias: "files", entry: "nothing", host: "files.k.example"},
	} {
		tenant := acmeTenantFixture()
		e := aliasedShares(c.alias, time.Now())
		e.ExposureName = c.entry
		tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{e}
		gw := buildPerimeterGateway("k.example", c.mode, "", "kernel", append([]gentianov1alpha1.Tenant{*tenant}, c.tenants...), perimeterProfiles())
		// id.k.example has a listener of the identity provider's own; what
		// must not exist is one made for the alias.
		if l := perimeterListener(gw, c.host); l != nil && string(l.Name) == perimeterListenerName(c.host) {
			t.Errorf("%s: the alias got a listener", name)
		}
	}
}

// Two surfaces that carry one alias: the one given it first answers there,
// and the other keeps only its own address.
func TestOneAliasIsOneSurfaces(t *testing.T) {
	first, second := acmeTenantFixture(), acmeTenantFixture()
	second.Name = "globex"
	first.Spec.Exposures = []gentianov1alpha1.TenantExposure{aliasedShares("files", time.Now().Add(-time.Hour))}
	second.Spec.Exposures = []gentianov1alpha1.TenantExposure{aliasedShares("files", time.Now())}
	in := mainAddressInputs{
		Tenants: []gentianov1alpha1.Tenant{*second, *first}, KernelDomain: "k.example", TenancyMode: "multi",
	}
	if got := aliasHostOf(in, "acme"); got != "files.k.example" {
		t.Fatalf("the first surface's alias = %q", got)
	}
	if got := aliasHostOf(in, "globex"); got != "" {
		t.Fatalf("the second surface was given the alias too: %q", got)
	}
}

func aliasHostOf(in mainAddressInputs, tenant string) string {
	live := []perimeterEnablement{{on: &gentianov1alpha1.ExposureEnablement{ExposureName: "shares", Alias: "files"}}}
	comp := &gentianov1alpha1.Component{}
	comp.Name = "nextcloud-base-ce"
	for i := range in.Tenants {
		if in.Tenants[i].Name == tenant {
			return aliasHostFor(in, &in.Tenants[i], comp, &live[0])
		}
	}
	return ""
}

// The route behind an aliased surface names both hosts, each on its own
// listener: one proxy, the same paths, two names.
func TestAnAliasedSurfaceIsRoutedAtBothNames(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{aliasedShares("files", time.Now())}
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	comp.Spec.Exposures = enabledShares(time.Now().Add(720 * time.Hour))
	comp.Spec.Exposures[0].Alias = "files"
	profile := nextcloudWithPerimeter()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", TenancyMode: "multi"}
	ctx := context.Background()

	if n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant)); err != nil || n != 1 {
		t.Fatalf("published %d, %v", n, err)
	}
	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{Namespace: layout.TenantDMZ(tenant.Name), Name: perimeterName(comp, "shares")}
	if err := c.Get(ctx, key, route); err != nil {
		t.Fatal(err)
	}
	var hosts, sections []string
	for _, h := range route.Spec.Hostnames {
		hosts = append(hosts, string(h))
	}
	for _, p := range route.Spec.ParentRefs {
		sections = append(sections, string(*p.SectionName))
	}
	if len(hosts) != 2 || hosts[0] != "share.acme.k.example" || hosts[1] != "files.k.example" {
		t.Fatalf("the route's hosts = %v", hosts)
	}
	if len(sections) != 2 || sections[1] != perimeterListenerName("files.k.example") {
		t.Fatalf("the route's listeners = %v", sections)
	}

	// The alias is taken away: the route is the surface's own again.
	stored := &gentianov1alpha1.Tenant{}
	if err := c.Get(ctx, types.NamespacedName{Name: tenant.Name}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Exposures[0].Alias = ""
	if err := c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	comp.Spec.Exposures[0].Alias = ""
	if _, err := r.ensurePerimeter(ctx, comp, profile, stored, r.zoneOf(stored)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, route); err != nil {
		t.Fatal(err)
	}
	if len(route.Spec.Hostnames) != 1 || len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("after the alias was removed the route still names %v", route.Spec.Hostnames)
	}
}
