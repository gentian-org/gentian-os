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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Opening a mailbox with a sign-in token is a declared requirement.
//
// The mail server validates a token by asking Keycloak's introspection
// endpoint as the client gentian-dovecot, and Keycloak (26.6 on) answers
// "active" only for a token that names the asking client in its audience. No
// token does unless something puts it there, so which apps' tokens open
// mailboxes is decided by who is given the audience -- and that is the apps
// whose profile declares requires.services.mail.imap.tokenSignIn.
//
// Three objects carry it, and this file writes none of them:
//
//   - the client scope "mailbox" in the tenant's realm, with the one mapper
//     that adds gentian-dovecot to an access token's audience
//     (tenant-default.yaml, beside the gentian-dovecot client);
//   - that scope as an OPTIONAL scope of the declaring app's own sign-in
//     client (app-default.yaml), from the fact this file puts on the claim;
//   - "scope = mailbox" in the mail server's check of each realm
//     (dovecotRealmAuthFiles), so the audience alone is not enough.
//
// Optional, so a token carries the scope and the audience only when the app
// asked for "mailbox" at that sign-in. The token the app holds for its own
// session, and relays to whatever it calls, does not open a mailbox.

// mailboxScopeName is the client scope the mail server requires of a token.
// The Compositions name it too; TestMailboxScopeHasOneName holds them
// together.
const mailboxScopeName = "mailbox"

// declaresMailboxTokenSignIn reports whether a profile asks that its sign-in
// tokens open mailboxes, and has the sign-in client to give the scope to.
func declaresMailboxTokenSignIn(profile *gentianov1alpha1.ComponentProfile) bool {
	s := profile.Services()
	if s == nil || s.Mail == nil || s.Mail.IMAP == nil || !s.Mail.IMAP.TokenSignIn {
		return false
	}
	return s.Identity != nil && s.Identity.OIDC != nil && s.Identity.OIDC.ClientID != ""
}

// tenantHasTokenMailboxes reports whether a tenant's people have mailboxes on
// this cluster's own mail server that a token of the tenant's realm can open.
//
// Three conditions, each of which is where one of the objects above is made:
// the cluster runs the mail server; the tenant is registered in it
// (selfhosted, which is also what a tenant that does not say gets on such a
// cluster); and the realm is the tenant's own. A tenant that adopts the kernel
// realm composes neither gentian-dovecot nor the scope there -- the realm is
// not its to write -- so there is nothing to give its apps.
func tenantHasTokenMailboxes(tenant *gentianov1alpha1.Tenant, clusterMailMode, kernelRealm string) bool {
	if clusterMailMode != mailServiceModeSystem {
		return false
	}
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" && tenant.Spec.Mail.Mode != gentianov1alpha1.MailModeSelfhosted {
		return false
	}
	return !tenantAdoptsKernelRealm(tenant, kernelRealm)
}

// mailboxTokenSignIn is the fact the App claim carries: this app declared it,
// and this tenant on this cluster can serve it. Where it cannot be served the
// declaration is accepted and does nothing, as requires.services.mail itself
// does on a cluster without the mail function.
//
// The cluster's mail mode is read from the claim's ConfigMap with no
// fallback: a cluster that cannot answer yet is not served until it can.
func (r *ComponentReconciler) mailboxTokenSignIn(
	ctx context.Context, tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile,
) bool {
	if !declaresMailboxTokenSignIn(profile) {
		return false
	}
	return tenantHasTokenMailboxes(tenant, clusterMailServiceMode(ctx, r.Client, ""), r.kernelRealm())
}
