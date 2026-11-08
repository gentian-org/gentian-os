/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Vouching (requires.services.vouching): a component signs a statement that
// names a person, and the tenant's realm answers with a token of that person
// for one app. The names below are what the tenant's Composition, the
// registrar and the component have to agree on, and are derived in one place.

// VouchingAlias is the component's name in the tenant's realm, twice over:
// the alias of the identity provider entry that makes it a trusted issuer,
// and the id of the client it asks with. A person is linked to the first.
func VouchingAlias(profile string) string { return "vouch-" + profile }

// VouchingIssuer is what the component's statements name as their issuer.
// It is a name and is never fetched: the keys are read from where the
// profile says they are published.
func VouchingIssuer(tenantNamespace, profile string) string {
	return "https://" + VouchingAlias(profile) + "." + tenantNamespace + ".svc"
}

// VouchingSecretName is the Secret of the tenant's namespace the component
// is told what it has to know in.
func VouchingSecretName(profile string) string { return "vouching-" + profile }

// The entries of that Secret.
const (
	VouchingIssuerKey       = "VOUCHING_ISSUER"
	VouchingClientIDKey     = "VOUCHING_CLIENT_ID"
	VouchingClientSecretKey = "VOUCHING_CLIENT_SECRET" //nolint:gosec // Secret key name, not a credential.
	VouchingTokenURLKey     = "VOUCHING_TOKEN_URL"     //nolint:gosec // Secret key name, not a credential.
)

func wantsVouching(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Vouching != nil
}
