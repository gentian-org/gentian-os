/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The store's sign-in through the tenant's own realm (Tenant
// spec.store.signIn, store-contract.md §6.1): one more client of the
// brokeredTo kind, broker-to-store, sending a person back to the store
// realm's broker endpoint for this tenant only, and carrying the person's
// identity and nothing else (identityOnly).
//
// The store's identity provider for the tenant is keyed by the tenant's
// metadata.uid, never by its name: a tenant deleted and made again under the
// same name is another tenant to the store, with another provider.

// storeClientName is the client's name: broker-to-store.
const storeClientName = "store"

// storeSignInClient is the XTenant's brokeredTo entry for the store, or nil
// while the tenant has not turned the sign-in on or has no uid yet.
func storeSignInClient(tenant *gentianov1alpha1.Tenant) map[string]interface{} {
	if tenant.Spec.Store == nil || tenant.Spec.Store.SignIn == nil || tenant.UID == "" {
		return nil
	}
	issuer := strings.TrimSuffix(tenant.Spec.Store.SignIn.Issuer, "/")
	if !strings.HasPrefix(issuer, "https://") {
		return nil
	}
	return map[string]interface{}{
		"name":         storeClientName,
		"redirectURI":  issuer + "/broker/org-" + string(tenant.UID) + "/endpoint",
		"jwksURL":      issuer + "/protocol/openid-connect/certs",
		"identityOnly": true,
	}
}
