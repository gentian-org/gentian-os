/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"sort"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What a component holds on people's behalf, read from its profile.
//
// A profile says this in several unrelated fields, each built for its own
// purpose. An administrator who wants to know what an installed app may do
// for the tenant's people should not have to read a profile to find out, so
// the fields are gathered here into one list, in terms a screen can word.
// Nothing is decided from this list: the fields themselves are what the
// operator acts on.

// The kinds of thing a component can hold.
const (
	// HoldSessionToken: the person's own session token is passed to the
	// component (expose[].forwardToken). It is valid at the platform's
	// services and at every other app of the tenant.
	HoldSessionToken = "session-token"
	// HoldAppToken: the component is handed a token of the person that is
	// valid at this app alone (expose[].exchangeToken).
	HoldAppToken = "app-token"
	// HoldAnyApp: the component can obtain a person's token for any app of
	// the tenant (requires.services.vouching at trustTier platform).
	HoldAnyApp = "any-app"
	// HoldHome: the component can obtain a person's token for the home of a
	// contract granted to it (integrations[].asPerson).
	HoldHome = "home"
	// HoldEveryHome: the component can obtain a person's token for each of
	// the tenant's homes (vouching.homes).
	HoldEveryHome = "every-home"
	// HoldDirector: a token the component obtains for a person may also be
	// used at the platform's own API, for the scopes named
	// (vouching.director).
	HoldDirector = "director"
	// HoldPlatformServices: the component is told the address of services in
	// the control namespace and admitted to them
	// (valueMapping.platform.*UrlKey).
	HoldPlatformServices = "platform-services"
	// HoldRightsCheck: the component may ask whether a person holds a right
	// (requires.services.rights).
	HoldRightsCheck = "rights-check"
	// HoldOwnStore: the component keeps rights of its own in a store the
	// platform makes for it (requires.services.authorization).
	HoldOwnStore = "own-store"
	// HoldPersonSecrets: the component keeps one secret per person
	// (requires.services.personSecrets).
	HoldPersonSecrets = "person-secrets"
	// HoldBridge: the component is a bridge with a way that is called as a
	// person (bridge.ways[].speaksAs: person).
	HoldBridge = "bridge-as-person"
)

// Whether the person has to be there.
const (
	// PresencePresent: only for a person who is signed in and at the app.
	PresencePresent = "present"
	// PresenceAway: also for a person who is not there.
	PresenceAway = "away"
)

// Hold is one thing a component holds on people's behalf.
type Hold struct {
	// Kind is which thing, one of the Hold constants.
	Kind string `json:"kind"`
	// Presence says whether the person has to be there, where the kind is
	// about acting for a person; empty where it is not.
	Presence string `json:"presence,omitempty"`
	// Names are what the hold concerns, as the profile names them: the
	// entries, contracts, services, scopes or ways. Sorted.
	Names []string `json:"names,omitempty"`
}

// holdsOf gathers what a profile holds on people's behalf. A profile that
// holds nothing answers with an empty list.
func holdsOf(profile *gentianov1alpha1.ComponentProfile) []Hold {
	holds := []Hold{}
	if profile == nil {
		return holds
	}
	add := func(kind, presence string, names []string) {
		sort.Strings(names)
		holds = append(holds, Hold{Kind: kind, Presence: presence, Names: names})
	}

	var forwarded, exchanged []string
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.ForwardToken {
			forwarded = append(forwarded, e.Name)
		}
		if e.ExchangeToken {
			exchanged = append(exchanged, e.Name)
		}
	}
	if len(forwarded) > 0 {
		add(HoldSessionToken, PresencePresent, forwarded)
	}
	if len(exchanged) > 0 {
		add(HoldAppToken, PresencePresent, exchanged)
	}

	services := profile.Services()
	var vouching *gentianov1alpha1.VouchingRequirement
	if services != nil {
		vouching = services.Vouching
	}
	// A component that names keys signs statements about people, so it acts
	// for a person who is away too. One that names none can only exchange
	// the token of a person who is there.
	presence := PresencePresent
	if vouching != nil && vouching.Keys != nil {
		presence = PresenceAway
	}
	if vouching != nil {
		// Below the platform's trust a component vouches towards its homes
		// alone: its client is given no app's scope.
		if profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform {
			add(HoldAnyApp, presence, nil)
		}
		if vouching.Homes {
			add(HoldEveryHome, presence, nil)
		}
		if len(vouching.Director) > 0 {
			scopes := make([]string, 0, len(vouching.Director))
			for _, d := range vouching.Director {
				scopes = append(scopes, string(d))
			}
			add(HoldDirector, presence, scopes)
		}
	}
	var contracts []string
	for i := range profile.Spec.Integrations {
		if profile.Spec.Integrations[i].AsPerson {
			contracts = append(contracts, profile.Spec.Integrations[i].Contract)
		}
	}
	if len(contracts) > 0 {
		add(HoldHome, presence, contracts)
	}

	if m := profile.Spec.Package.ValueMapping; m != nil && m.Platform != nil {
		var reached []string
		for name, key := range map[string]string{
			"director":  m.Platform.DirectorURLKey,
			"usher":     m.Platform.UsherURLKey,
			"custodian": m.Platform.CustodianURLKey,
			"registrar": m.Platform.RegistrarURLKey,
		} {
			if key != "" {
				reached = append(reached, name)
			}
		}
		if len(reached) > 0 {
			add(HoldPlatformServices, "", reached)
		}
	}

	if services != nil {
		if services.Rights != nil {
			var asks []string
			if services.Rights.Audit {
				asks = append(asks, "audit")
			}
			add(HoldRightsCheck, "", asks)
		}
		if services.Authorization != nil {
			var with []string
			if services.Authorization.Groups == gentianov1alpha1.AuthorizationGroupsMirror {
				with = append(with, "groups")
			}
			add(HoldOwnStore, "", with)
		}
		if services.PersonSecrets != nil {
			add(HoldPersonSecrets, "", nil)
		}
	}

	if bridge := profile.Spec.Bridge; bridge != nil {
		var ways []string
		for i := range bridge.Ways {
			if bridge.Ways[i].SpeaksAs == gentianov1alpha1.BridgeSpeakerPerson {
				ways = append(ways, bridge.Ways[i].Name)
			}
		}
		if len(ways) > 0 {
			add(HoldBridge, "", ways)
		}
	}
	return holds
}
