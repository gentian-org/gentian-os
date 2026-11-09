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
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Who may send as which address.
//
// Postfix refuses a signed-in client whose envelope sender it does not own
// (reject_authenticated_sender_login_mismatch), and this table says who owns
// what. Without it any credential the cluster hands out -- a person's, an
// app's, a realm's -- could send as any address of any tenant.
//
// The table is a list of addresses or whole domains, each with the logins that
// own it, rendered as a pcre: map in order: single addresses first, then
// domains. Postfix takes the first match. An address in no domain this cluster
// hosts has no owner, so it is refused: the cluster sends only for its own
// domains.
//
// The owners, as the platform hands out credentials today:
//
//   - a person signs in with their address (the realm's email, which Dovecot
//     reports as the login for XOAUTH2 and per-app passwords), so every address
//     of a hosted domain is owned by the login of the same name;
//   - a tenant's apps share the login smtp-<tenant> and send from their own
//     address in the tenant's domain (docmost@<domain>), so that login owns the
//     whole domain;
//   - a tenant realm signs in as noreply@<its domain> and sends from
//     noreply@<kernelDomain>, as does the kernel realm with the identity the
//     installer gave it, so that one address has those owners as well.
//
// The cluster's mail tenant (mainAddress.mailTenant) is one more owner of the
// cluster's domain: its people send as <local>@<kernelDomain> as well as from
// their own address (christian@team.example.org as christian@example.org).
// A per-person address later is one more entry of the same list, not another
// mechanism.
const postfixSenderLoginKey = "sender_login"

// mailAddressOwner is one entry of the table: an address, or "@domain" for every
// address of a domain, and the logins that may send as it. A login may contain
// "$1", which stands for the local part of the address being sent from.
type mailAddressOwner struct {
	address string
	logins  []string
}

// smtpAppLogin is the login a tenant's apps share for submission.
func smtpAppLogin(tenant string) string { return "smtp-" + tenant }

// senderOwners derives the table from the mail registry (registry key to mail
// domain; the kernel's own entry is kernelMailRegistryKey) and the login the
// kernel realm submits with, which may be empty.
func senderOwners(registry map[string]string, kernelDomain, kernelRealmLogin, mailTarget string) []mailAddressOwner {
	domains := map[string][]string{}
	noreplyOwners := []string{}
	add := func(list []string, v string) []string {
		for _, x := range list {
			if x == v {
				return list
			}
		}
		return append(list, v)
	}
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d := strings.ToLower(strings.TrimSpace(registry[key]))
		if d == "" {
			continue
		}
		domains[d] = add(domains[d], "$1@"+d)
		if key != kernelMailRegistryKey {
			domains[d] = add(domains[d], smtpAppLogin(key))
			noreplyOwners = add(noreplyOwners, mailSubmissionLocalPart+"@"+d)
		}
	}
	kernelDomain = strings.ToLower(kernelDomain)
	var exact []mailAddressOwner
	if kernelDomain != "" {
		domains[kernelDomain] = add(domains[kernelDomain], "$1@"+kernelDomain)
		if mailTarget != "" {
			domains[kernelDomain] = add(domains[kernelDomain], "$1@"+mailTarget)
		}
		owners := []string{mailSubmissionLocalPart + "@" + kernelDomain}
		if kernelRealmLogin != "" {
			owners = add(owners, strings.ToLower(kernelRealmLogin))
		}
		for _, o := range noreplyOwners {
			owners = add(owners, o)
		}
		exact = append(exact, mailAddressOwner{address: mailSubmissionLocalPart + "@" + kernelDomain, logins: owners})
	}
	names := make([]string, 0, len(domains))
	for d := range domains {
		names = append(names, d)
	}
	sort.Strings(names)
	out := exact
	for _, d := range names {
		out = append(out, mailAddressOwner{address: "@" + d, logins: domains[d]})
	}
	return out
}

// renderSenderLoginMap writes the table as Postfix's pcre: map. PCRE tables
// match case-insensitively by default, which is what an address wants.
func renderSenderLoginMap(owners []mailAddressOwner) string {
	var b strings.Builder
	for _, o := range owners {
		var pattern string
		if strings.HasPrefix(o.address, "@") {
			pattern = "^([^@]+)@" + regexp.QuoteMeta(o.address[1:]) + "$"
		} else {
			pattern = "^" + regexp.QuoteMeta(o.address) + "$"
		}
		logins := make([]string, len(o.logins))
		for i, l := range o.logins {
			logins[i] = strings.ReplaceAll(l, "$1", "${1}")
		}
		fmt.Fprintf(&b, "/%s/ %s\n", pattern, strings.Join(logins, ","))
	}
	return b.String()
}

// kernelRealmSubmissionLogin is the login the kernel realm submits with, as the
// installer wrote it; empty when there is none.
func (r *TenantReconciler) kernelRealmSubmissionLogin(ctx context.Context) (string, error) {
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: keycloakSMTPCredentialsSecret, Namespace: identityNamespace}, sec)
	if err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return strings.TrimSpace(string(sec.Data["smtp_user"])), nil
}

// kernelDomainMailTarget is the mail domain of the tenant the cluster names
// for its own domain's mail, or "" where it names none or one that cannot
// take it: a tenant that has no mail domain here, or whose mail domain is
// the cluster's already (the user tenant of a single-tenancy cluster, and
// the kernel realm's people, who are addressed at the cluster's domain).
func kernelDomainMailTarget(registry map[string]string, tenant, kernelDomain string) string {
	if tenant == "" || tenant == kernelMailRegistryKey {
		return ""
	}
	d := strings.ToLower(strings.TrimSpace(registry[tenant]))
	if d == "" || d == strings.ToLower(kernelDomain) {
		return ""
	}
	return d
}

// kernelDomainDelivery are the virtual_alias lines that deliver the cluster's
// domain to its mail tenant's mailboxes: every address of the domain to the
// same local part at the tenant's domain, after the addresses that keep
// their own mailbox.
//
// A domain-wide alias rewrites every address of the domain, so whoever is
// addressed at the cluster's domain already -- the kernel realm's people --
// is mapped to themselves first: virtual(5) looks the whole address up
// before the domain, and an address mapped to itself is not rewritten.
// taken are the addresses the alias file names already (the role addresses,
// the configured forwards), which keep their own line.
func kernelDomainDelivery(kernelDomain, target string, kernelPeople []string, taken map[string]bool) string {
	var b strings.Builder
	kernelDomain = strings.ToLower(kernelDomain)
	for _, a := range kernelPeople {
		a = strings.ToLower(a)
		if !strings.HasSuffix(a, "@"+kernelDomain) || taken[a] {
			continue
		}
		taken[a] = true
		fmt.Fprintf(&b, "%s %s\n", a, a)
	}
	fmt.Fprintf(&b, "@%s @%s\n", kernelDomain, target)
	return b.String()
}
