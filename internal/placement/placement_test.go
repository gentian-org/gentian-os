/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package placement_test

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/placement"
)

func profile(name string, mutate func(*gentianov1alpha1.ComponentProfile)) gentianov1alpha1.ComponentProfile {
	p := gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{}},
		Spec:       gentianov1alpha1.ComponentProfileSpec{Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}},
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

func placed(tenants gentianov1alpha1.PlacementTenants, while gentianov1alpha1.PlacementCondition) func(*gentianov1alpha1.ComponentProfile) {
	return func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: tenants, While: while}
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// Each kind of tenant has what the profiles place there, and nothing a
// profile does not place: an installable profile, one of another class, one
// placed while the cluster serves models (the worker finds that in the
// catalog's index), one gone from the catalog, and one that needs usage
// statistics on a cluster that does not report.
func TestEachTenantHasWhatTheProfilesPlaceThere(t *testing.T) {
	profiles := []gentianov1alpha1.ComponentProfile{
		profile("desktop", placed(gentianov1alpha1.PlacementEvery, "")),
		profile("concierge", placed(gentianov1alpha1.PlacementPlatform, "")),
		profile("store", placed(gentianov1alpha1.PlacementUsers, gentianov1alpha1.PlacementWhileStoreOffered)),
		profile("chat", placed(gentianov1alpha1.PlacementUsers, gentianov1alpha1.PlacementWhileLLM)),
		profile("notes", nil),
		profile("shared", func(p *gentianov1alpha1.ComponentProfile) {
			placed(gentianov1alpha1.PlacementEvery, "")(p)
			p.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassService}
		}),
		profile("retired", placed(gentianov1alpha1.PlacementEvery, "")),
		profile("reporting", func(p *gentianov1alpha1.ComponentProfile) {
			placed(gentianov1alpha1.PlacementPlatform, "")(p)
			p.Annotations[placement.RequiresUsageStatisticsAnnotation] = "true"
		}),
	}
	got := placement.Decide(placement.Facts{Profiles: profiles, StoreOffered: yes(), Gone: map[string]bool{"retired": true}})
	if want := []string{"desktop", "store"}; !reflect.DeepEqual(got.Users.Wanted, want) {
		t.Errorf("a user tenant has %v, want %v", got.Users.Wanted, want)
	}
	if want := []string{"concierge", "desktop"}; !reflect.DeepEqual(got.Platform.Wanted, want) {
		t.Errorf("the platform tenant has %v, want %v", got.Platform.Wanted, want)
	}

	reporting := placement.Decide(placement.Facts{Profiles: profiles, UsageStatistics: true, StoreOffered: no(), Gone: map[string]bool{"retired": true}})
	if want := []string{"concierge", "desktop", "reporting"}; !reflect.DeepEqual(reporting.Platform.Wanted, want) {
		t.Errorf("on a reporting cluster the platform tenant has %v, want %v", reporting.Platform.Wanted, want)
	}
	if want := []string{"desktop"}; !reflect.DeepEqual(reporting.Users.Wanted, want) {
		t.Errorf("without a store a user tenant has %v, want %v", reporting.Users.Wanted, want)
	}
}

// Whether a store is offered could not be read: what is placed while it is
// is undecided, which neither places nor removes it.
func TestAStoreThatCannotBeReadDecidesNothing(t *testing.T) {
	profiles := []gentianov1alpha1.ComponentProfile{
		profile("store", placed(gentianov1alpha1.PlacementUsers, gentianov1alpha1.PlacementWhileStoreOffered)),
	}
	got := placement.Decide(placement.Facts{Profiles: profiles})
	if got.Users.Has("store") || got.Users.Decided("store") {
		t.Errorf("users = %+v, want the store undecided", got.Users)
	}
}

// A catalog published before placement keeps working: each deprecated flag
// is the placement it always meant.
func TestTheDeprecatedFlagsAreTheirPlacement(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*gentianov1alpha1.ComponentProfile)
		want   *gentianov1alpha1.ComponentPlacement
	}{
		"defaultForTenants":        {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.DefaultForTenants = true }, &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}},
		"defaultForPlatform":       {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.DefaultForPlatform = true }, &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementPlatform}},
		"defaultWhereStoreOffered": {func(p *gentianov1alpha1.ComponentProfile) { p.Spec.DefaultWhereStoreOffered = true }, &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementUsers, While: gentianov1alpha1.PlacementWhileStoreOffered}},
		"requires-capability": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Annotations[gentianov1alpha1.RequiresCapabilityAnnotation] = "llm"
		}, &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementUsers, While: gentianov1alpha1.PlacementWhileLLM}},
		"placement wins": {func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.DefaultForTenants = true
			placed(gentianov1alpha1.PlacementPlatform, "")(p)
		}, &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementPlatform}},
		"installable": {nil, nil},
	} {
		p := profile("x", tc.mutate)
		if got := p.PlacementOf(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: placement %+v, want %+v", name, got, tc.want)
		}
	}
}

// The readers of spec.apps that serve installed apps leave out what the
// platform places itself, and keep an entry placed with the cluster's
// models, one that is not placed, and one whose profile is not there.
func TestInstalledLeavesOutWhatThePlatformPlacesItself(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	desktop := profile("desktop", placed(gentianov1alpha1.PlacementEvery, ""))
	chat := profile("chat", placed(gentianov1alpha1.PlacementUsers, gentianov1alpha1.PlacementWhileLLM))
	notes := profile("notes", nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&desktop, &chat, &notes).Build()
	apps := []gentianov1alpha1.TenantApp{
		{Profile: "desktop", Placed: true},
		{Profile: "chat", Placed: true},
		{Profile: "notes"},
		{Profile: "gone", Placed: true},
	}
	got, err := placement.Installed(context.Background(), c, apps)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Profile)
	}
	if want := []string{"chat", "notes", "gone"}; !reflect.DeepEqual(names, want) {
		t.Errorf("installed = %v, want %v", names, want)
	}
}

// The platform pre-approves a contract between two components it places
// itself, both of platform trust, where the consumer names the provider and
// what it asks of it and the provider provides it -- and nothing else.
func TestThePlatformPreApprovesContractsBetweenItsOwnComponents(t *testing.T) {
	platform := func(p *gentianov1alpha1.ComponentProfile) { p.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform }
	provides := func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Provides = []gentianov1alpha1.ContractRef{{Name: "desktop-api"}}
	}
	consumes := func(provider string, caps ...string) func(*gentianov1alpha1.ComponentProfile) {
		return func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Integrations = append(p.Spec.Integrations, gentianov1alpha1.IntegrationRef{Contract: "desktop-api", Provider: provider, Capabilities: caps})
		}
	}
	all := func(fs ...func(*gentianov1alpha1.ComponentProfile)) func(*gentianov1alpha1.ComponentProfile) {
		return func(p *gentianov1alpha1.ComponentProfile) {
			for _, f := range fs {
				f(p)
			}
		}
	}
	every := placed(gentianov1alpha1.PlacementEvery, "")
	users := placed(gentianov1alpha1.PlacementUsers, gentianov1alpha1.PlacementWhileStoreOffered)
	profiles := []gentianov1alpha1.ComponentProfile{
		profile("desktop", all(every, platform, provides)),
		profile("store", all(users, platform, consumes("desktop", "tiles"))),
		// Asks for nothing in particular: nothing to grant.
		profile("vague", all(every, platform, consumes("desktop"))),
		// Not of platform trust.
		profile("certified", all(every, consumes("desktop", "tiles"))),
		// A tenant installs it.
		profile("installed", all(platform, consumes("desktop", "tiles"))),
		// Names no provider.
		profile("anyone", all(every, platform, consumes("", "tiles"))),
	}
	got := placement.Decide(placement.Facts{Profiles: profiles, StoreOffered: yes()}).Grants
	want := []placement.Grant{{App: "store", Contract: "desktop-api", Provider: "desktop", Capabilities: []string{"tiles"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grants = %+v, want %+v", got, want)
	}
}

// A component placed on a credential is placed once the platform tenant holds
// it, and is never withdrawn for lacking it: what it keeps outlives the
// credential.
func TestACredentialPlacesAComponentOneWay(t *testing.T) {
	profiles := []gentianov1alpha1.ComponentProfile{
		profile("vault", placed(gentianov1alpha1.PlacementPlatform, gentianov1alpha1.PlacementWhileCredentialSet)),
	}
	set := placement.Decide(placement.Facts{Profiles: profiles, Credentialed: map[string]bool{"vault": true}}).For(true)
	if !set.Has("vault") || !set.Decided("vault") {
		t.Fatalf("with its credential: %+v", set)
	}
	set = placement.Decide(placement.Facts{Profiles: profiles}).For(true)
	if set.Has("vault") || set.Decided("vault") {
		t.Fatalf("without it the component is neither placed nor withdrawn: %+v", set)
	}
	if users := placement.Decide(placement.Facts{Profiles: profiles, Credentialed: map[string]bool{"vault": true}}).For(false); users.Has("vault") {
		t.Fatalf("a user tenant was given it: %+v", users)
	}
}
