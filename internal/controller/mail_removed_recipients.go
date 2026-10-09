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
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// Mail to a removed person's address is refused.
//
// Under the recipient policy catchall, the default, every address of a hosted
// domain is accepted, so mail to somebody who was removed went on arriving:
// it made a new directory under the old name, and whoever was given the
// address later found it. Under strict the address drops out of the list with
// the person, but strict keeps the catch-all for a domain whenever its list
// cannot be built, and then the same happened.
//
// What says a person was removed is the record of what became of their
// mailbox (MailboxRemoval), archived or deleted alike. An address is refused
// for as long as such a record stands and nobody holds the address:
//
//   - the record is not one the operator refused, and its address is a plain
//     name in the mail domain its tenant has now -- what the operator asks of
//     a record before it touches a mailbox, asked again here;
//   - nobody of any realm whose people have addresses in that domain holds
//     it, switched off or not. While the person is still there nothing is
//     refused, and an address given to somebody new receives again;
//   - abuse@, dmarc@ and postmaster@ are never refused.
//
// A record of a deleted mailbox is removed after mailboxRecordKept, and the
// refusal ends with it: the address is then one nobody owns, and the
// recipient policy decides as it does for any other.
//
// The answer is a permanent one, 550, and a wrong one loses mail. So where
// the people of a domain cannot be listed, nothing about that domain's
// addresses changes: those refused before stay refused, and no other is
// added.

// removedRecipientAnswer is what Postfix answers at RCPT for such an address:
// "550 5.1.1 <address>: Recipient address rejected: User unknown".
const removedRecipientAnswer = "550 5.1.1 User unknown"

// removedRecipients renders the lines that refuse the addresses of removed
// people in the domains Postfix accepts mail for, one "<address> <answer>"
// each, sorted. It answers no error: whatever cannot be read leaves the
// lines of the map as it stands, and the domains beside them are written
// either way.
func (r *TenantReconciler) removedRecipients(ctx context.Context, hosted map[string]bool) string {
	logger := log.FromContext(ctx)
	standing := r.standingRemovedRecipients(ctx)
	keep := func(domain string) []string {
		var out []string
		for _, address := range standing {
			if strings.HasSuffix(address, "@"+domain) {
				out = append(out, address)
			}
		}
		return out
	}
	render := func(addresses []string) string {
		sort.Strings(addresses)
		var b strings.Builder
		for _, address := range slices.Compact(addresses) {
			fmt.Fprintf(&b, "%s %s\n", address, removedRecipientAnswer)
		}
		return b.String()
	}
	accepted := make(map[string]bool, len(hosted))
	hostedDomains := make([]string, 0, len(hosted))
	for domain := range hosted {
		accepted[strings.ToLower(domain)] = true
		hostedDomains = append(hostedDomains, strings.ToLower(domain))
	}
	sort.Strings(hostedDomains)
	keepAll := func() string {
		var all []string
		for _, domain := range hostedDomains {
			all = append(all, keep(domain)...)
		}
		return render(all)
	}

	records := &gentianov1alpha1.MailboxRemovalList{}
	if err := r.List(ctx, records); err != nil {
		logger.Error(err, "the records of removed people's mailboxes could not be listed; the addresses refused stay as they are")
		return keepAll()
	}
	if len(records.Items) == 0 {
		return ""
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		logger.Error(err, "the tenants could not be listed; the addresses refused stay as they are")
		return keepAll()
	}
	mailboxDomain := map[string]string{}
	for i := range tenants.Items {
		mailboxDomain[tenants.Items[i].Name] = strings.ToLower(r.mailboxDomainOf(ctx, &tenants.Items[i]))
	}

	removed := map[string][]string{}
	for i := range records.Items {
		record := &records.Items[i]
		if record.Status.Refused || record.DeletionTimestamp != nil {
			continue
		}
		name, domain, ok := cutAddress(strings.ToLower(record.Spec.Address))
		if !ok || !accepted[domain] || domain != mailboxDomain[record.Spec.Tenant] ||
			backup.ValidMailboxName(name) != nil || slices.Contains(mailboxLocalParts, name) {
			continue
		}
		removed[domain] = append(removed[domain], name+"@"+domain)
	}

	var refused []string
	for _, domain := range hostedDomains {
		if len(removed[domain]) == 0 {
			continue
		}
		held, err := r.mailDomainHolders(ctx, tenants, domain)
		if err != nil {
			logger.Error(err, "the people of a mail domain could not be listed; its removed people's addresses stay refused or accepted as they are",
				"domain", domain)
			refused = append(refused, keep(domain)...)
			continue
		}
		for _, address := range removed[domain] {
			if !held[address] {
				refused = append(refused, address)
			}
		}
	}
	return render(refused)
}

// standingRemovedRecipients are the addresses the map Postfix reads refuses
// now.
func (r *TenantReconciler) standingRemovedRecipients(ctx context.Context) []string {
	maps := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: postfixVirtualMailboxMapsConfigMap, Namespace: postfixNamespace}, maps); err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(maps.Data[postfixVirtualMailboxDomainsKey], "\n") {
		if address, answer, ok := strings.Cut(line, " "); ok && answer == removedRecipientAnswer && strings.Contains(address, "@") {
			out = append(out, address)
		}
	}
	return out
}

// mailDomainHolders is every address held by a person of any realm whose
// people have addresses in a mail domain.
func (r *TenantReconciler) mailDomainHolders(ctx context.Context, tenants *gentianov1alpha1.TenantList, domain string) (map[string]bool, error) {
	held := map[string]bool{}
	for _, realm := range r.realmsOfMailDomainAmong(tenants, domain) {
		people, err := r.keycloakRealmHolders(ctx, realm)
		if err != nil {
			return nil, fmt.Errorf("realm %s: %w", realm, err)
		}
		for address := range people {
			held[address] = true
		}
	}
	return held, nil
}

// realmsOfMailDomain is every realm whose people have addresses in a mail
// domain: the realm of each tenant that has its mailboxes under it, and the
// kernel realm for the cluster's own domain. On a cluster with one user
// tenant that is more than one realm for one domain, and so more than one
// place a mailbox's person can be.
func (r *TenantReconciler) realmsOfMailDomain(ctx context.Context, domain string) ([]string, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return nil, err
	}
	return r.realmsOfMailDomainAmong(tenants, domain), nil
}

func (r *TenantReconciler) realmsOfMailDomainAmong(tenants *gentianov1alpha1.TenantList, domain string) []string {
	seen := map[string]bool{}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if strings.EqualFold(mailDomain(t, r.KernelDomain, r.TenancyMode), domain) {
			seen[keycloakRealmName(t)] = true
		}
	}
	if r.KernelRealm != "" && strings.EqualFold(domain, r.KernelDomain) {
		seen[r.KernelRealm] = true
	}
	realms := make([]string, 0, len(seen))
	for realm := range seen {
		realms = append(realms, realm)
	}
	sort.Strings(realms)
	return realms
}
