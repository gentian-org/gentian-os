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
	"slices"
	"sort"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Opening a mailbox with a sign-in token is a declared requirement.
//
// The mail server checks a token itself, against the realm's keys, and
// accepts it only when its audience names gentian-dovecot. No token does
// unless something puts it there, so which apps' tokens open mailboxes is
// decided by who is given the audience -- and that is the apps whose profile
// declares requires.services.mail.imap.tokenSignIn.
//
// Three objects carry it, and this file writes none of them:
//
//   - the client scope "mailbox" in the tenant's realm, with the one mapper
//     that adds gentian-dovecot to an access token's audience
//     (tenant-default.yaml, beside the gentian-dovecot client);
//   - that scope as an OPTIONAL scope of the declaring app's own sign-in
//     client (apprender), from the fact this file puts on the render;
//   - "scope = mailbox" in the mail server's check (dovecotOAuth2Conf), so the
//     audience alone is not enough.
//
// Optional, so a token carries the scope and the audience only when the app
// asked for "mailbox" at that sign-in. The token the app holds for its own
// session, and relays to whatever it calls, does not open a mailbox.
//
// A bridge signs nobody in and has no client. Where a bridge declares it,
// the scope is an optional scope of the clients of the tenant's components
// that vouch for people (mailboxVouchers): one of them obtains the person's
// token for a call to the bridge, asks for "mailbox" with it because the
// bridge's entry on its list says so (agentBridgeListEntry.Scopes), and the
// bridge passes that token on to the mail server.

// mailboxScopeName is the client scope the mail server requires of a token.
// The Compositions name it too; TestMailboxScopeHasOneName holds them
// together.
const mailboxScopeName = "mailbox"

// declaresMailboxTokenSignIn reports whether a profile asks that its sign-in
// tokens open mailboxes, and has the sign-in client to give the scope to. A
// bridge has none (bridgeOpensMailboxes).
func declaresMailboxTokenSignIn(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile.Spec.Bridge != nil || !asksForMailboxTokens(profile) {
		return false
	}
	s := profile.Services()
	return s.Identity != nil && s.Identity.OIDC != nil && s.Identity.OIDC.ClientID != ""
}

// bridgeOpensMailboxes reports whether a profile is a bridge that opens a
// person's mailbox with the token a call to it carries.
func bridgeOpensMailboxes(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Spec.Bridge != nil && asksForMailboxTokens(profile)
}

func asksForMailboxTokens(profile *gentianov1alpha1.ComponentProfile) bool {
	s := profile.Services()
	return s != nil && s.Mail != nil && s.Mail.IMAP != nil && s.Mail.IMAP.TokenSignIn
}

// mailboxVouchers are, among the profiles of one tenant's components, the
// ones whose client may ask for "mailbox": every component that vouches for
// people with the platform's trust (vouchesNarrowly says which do not),
// while the tenant has a bridge that opens mailboxes. Without such a bridge
// nobody is given the scope. Sorted.
func mailboxVouchers(profiles []*gentianov1alpha1.ComponentProfile) []string {
	bridge := false
	var out []string
	for _, p := range profiles {
		switch {
		case bridgeOpensMailboxes(p):
			bridge = true
		case p != nil && wantsVouching(p) && !vouchesNarrowly(p):
			out = append(out, p.Name)
		}
	}
	if !bridge {
		return nil
	}
	sort.Strings(out)
	return slices.Compact(out)
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
	if canonicalMailServiceMode(clusterMailMode) != mailServiceModeInternal {
		return false
	}
	if tenant.Spec.Mail != nil && tenant.Spec.Mail.Mode != "" && tenant.Spec.Mail.Mode != gentianov1alpha1.MailModeSelfhosted {
		return false
	}
	return !tenantAdoptsKernelRealm(tenant, kernelRealm)
}

// mailboxTokenSignIn is the fact the render is given (and an App claim
// carries, for a profile's own Composition): this app declared it,
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
