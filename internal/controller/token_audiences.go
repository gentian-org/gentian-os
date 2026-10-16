/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// An app's sign-in tokens name a service in their audience when the profile
// says so: requires.services.identity.oidc.audiences. The app Composition
// puts one mapper per name on the app's client.
//
// It is a grant. A service that checks its name in aud accepts every token
// that carries it, whoever the client was, so a name given to one app's
// client is the key to whatever that service holds. It is therefore given
// only to a profile from a catalogue of the whole cluster, which the
// cluster's operator chose to trust, and never from a tenant's own catalogue.
// The Composition holds the same rule; this is what tells the tenant why.

// reasonTokenAudiencesRefused is the condition reason of an install held by
// the rule above.
const reasonTokenAudiencesRefused = "TokenAudiencesRefused"

// tokenAudiencesRefusal answers why a profile's audiences may not be given,
// or "" when it declares none or they may.
func tokenAudiencesRefusal(profile *gentianov1alpha1.ComponentProfile) string {
	s := profile.Services()
	if s == nil || s.Identity == nil || s.Identity.OIDC == nil || len(s.Identity.OIDC.Audiences) == 0 {
		return ""
	}
	origin, err := profilebundle.ParseOrigin(profile.Annotations[profilebundle.OriginAnnotation])
	if err == nil && origin.Source != "" && origin.Tenant == "" {
		return ""
	}
	return fmt.Sprintf("ComponentProfile %q declares requires.services.identity.oidc.audiences: "+
		"a service accepts every token that names it, so its name is given only to a profile from a catalogue of the whole cluster, and this profile is not from one",
		profile.Name)
}
