/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"reflect"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestAProfileThatHoldsNothingForPeopleSaysSo(t *testing.T) {
	if got := holdsOf(&gentianov1alpha1.ComponentProfile{}); len(got) != 0 {
		t.Fatalf("holds = %+v, want none", got)
	}
	if got := holdsOf(nil); got == nil || len(got) != 0 {
		t.Fatalf("holds of no profile = %#v, want an empty list", got)
	}
}

func TestWhatAProfileHoldsForPeople(t *testing.T) {
	keys := &gentianov1alpha1.VouchingKeys{Service: "svc", Port: 8443}
	for _, tc := range []struct {
		name string
		spec gentianov1alpha1.ComponentProfileSpec
		want []Hold
	}{
		{
			name: "a console of the platform is passed the session's token and reaches the control namespace",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierPlatform,
				Expose:    []gentianov1alpha1.ExposureSpec{{Name: "web", ForwardToken: true}, {Name: "plain"}},
				Package: gentianov1alpha1.PackageSpec{ValueMapping: &gentianov1alpha1.ValueMapping{
					Platform: &gentianov1alpha1.PlatformValueMapping{UsherURLKey: "u", DirectorURLKey: "d"},
				}},
			},
			want: []Hold{
				{Kind: HoldSessionToken, Presence: PresencePresent, Names: []string{"web"}},
				{Kind: HoldPlatformServices, Names: []string{"director", "usher"}},
			},
		},
		{
			name: "an app is handed a token valid at itself",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierCertified,
				Expose:    []gentianov1alpha1.ExposureSpec{{Name: "web", ExchangeToken: true}},
			},
			want: []Hold{{Kind: HoldAppToken, Presence: PresencePresent, Names: []string{"web"}}},
		},
		{
			name: "a platform component that signs statements acts at any app, for people who are away too",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierPlatform,
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Vouching: &gentianov1alpha1.VouchingRequirement{
						Keys: keys, Homes: true,
						Director: []gentianov1alpha1.DirectorScope{"plans-read", "plans-apply"},
					},
				}},
			},
			want: []Hold{
				{Kind: HoldAnyApp, Presence: PresenceAway},
				{Kind: HoldEveryHome, Presence: PresenceAway},
				{Kind: HoldDirector, Presence: PresenceAway, Names: []string{"plans-apply", "plans-read"}},
			},
		},
		{
			name: "a platform component that only exchanges acts for people who are there",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierPlatform,
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Vouching: &gentianov1alpha1.VouchingRequirement{Exchange: true},
				}},
			},
			want: []Hold{{Kind: HoldAnyApp, Presence: PresencePresent}},
		},
		{
			name: "a certified app with keys reaches its homes and no app, for people who are away too",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierCertified,
				Integrations: []gentianov1alpha1.IntegrationRef{
					{Contract: "file-store", AsPerson: true}, {Contract: "calendar-store", AsPerson: true}, {Contract: "mail"},
				},
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Vouching: &gentianov1alpha1.VouchingRequirement{Keys: keys},
				}},
			},
			want: []Hold{{Kind: HoldHome, Presence: PresenceAway, Names: []string{"calendar-store", "file-store"}}},
		},
		{
			name: "a certified app without keys reaches its home only while the person is there",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier:    gentianov1alpha1.TrustTierCertified,
				Expose:       []gentianov1alpha1.ExposureSpec{{Name: "web", ExchangeToken: true}},
				Integrations: []gentianov1alpha1.IntegrationRef{{Contract: "file-store", AsPerson: true}},
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Vouching: &gentianov1alpha1.VouchingRequirement{Exchange: true},
				}},
			},
			want: []Hold{
				{Kind: HoldAppToken, Presence: PresencePresent, Names: []string{"web"}},
				{Kind: HoldHome, Presence: PresencePresent, Names: []string{"file-store"}},
			},
		},
		{
			name: "rights checks, a store of its own, secrets per person and a bridge called as a person",
			spec: gentianov1alpha1.ComponentProfileSpec{
				TrustTier: gentianov1alpha1.TrustTierPlatform,
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Rights:        &gentianov1alpha1.RightsRequirement{Audit: true},
					Authorization: &gentianov1alpha1.AuthorizationRequirement{Groups: gentianov1alpha1.AuthorizationGroupsMirror},
					PersonSecrets: &gentianov1alpha1.PersonSecretsRequirement{},
				}},
				Bridge: &gentianov1alpha1.BridgeSpec{Ways: []gentianov1alpha1.BridgeWay{
					{Name: "tools", SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson},
					{Name: "feed", SpeaksAs: gentianov1alpha1.BridgeSpeakerTenant},
				}},
			},
			want: []Hold{
				{Kind: HoldRightsCheck, Names: []string{"audit"}},
				{Kind: HoldOwnStore, Names: []string{"groups"}},
				{Kind: HoldPersonSecrets},
				{Kind: HoldBridge, Names: []string{"tools"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := holdsOf(&gentianov1alpha1.ComponentProfile{Spec: tc.spec})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("holds:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
