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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Mail to a removed person's address is refused: what Postfix is told, from
// the record the registrar writes to the map Postfix reads.

// mailRegistry is the registry of mail domains, as the mail reconciler keeps
// it: tenant name to domain.
func mailRegistry(domains map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: mailPostfixVirtualDomainsConfigMap, Namespace: mailNamespace},
		Data:       domains,
	}
}

// postfixMaps writes the maps again and answers what Postfix would read.
func (w *mailboxWorld) postfixMaps() map[string]string {
	w.t.Helper()
	if err := w.tr.syncPostfixVirtualMailboxMaps(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	maps := &corev1.ConfigMap{}
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: postfixVirtualMailboxMapsConfigMap, Namespace: postfixNamespace}, maps); err != nil {
		w.t.Fatal(err)
	}
	return maps.Data
}

func refusal(address string) string { return address + " 550 5.1.1 User unknown\n" }

// Archived or deleted, the address is refused from the moment the person is
// gone -- before the mailbox is touched -- and not while they are still
// there. The domain goes on accepting everybody else, the catch-all stays,
// and nothing about who may send changes.
func TestMailToARemovedPersonsAddressIsRefused(t *testing.T) {
	for _, choice := range []gentianov1alpha1.MailboxChoice{gentianov1alpha1.MailboxChoiceArchive, gentianov1alpha1.MailboxChoiceDelete} {
		t.Run(string(choice), func(t *testing.T) {
			w := newMailboxWorld(t, selfhosted("demo"), mailRegistry(map[string]string{"demo": "demo.k.example"}),
				mailboxRecord("demo-r1", "demo", "Alice@demo.k.example", choice))
			w.people.set("demo", person("alice@demo.k.example", true), person("bob@demo.k.example", true))

			// The record is written first, and the person removed next.
			w.reconcile("demo-r1")
			if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != "demo.k.example OK\n" {
				t.Fatalf("while the person is there: %q", got)
			}

			w.people.set("demo", person("bob@demo.k.example", true))
			record := w.reconcile("demo-r1")
			if len(w.jobs()) != 0 {
				t.Fatalf("the mailbox was touched at once: %+v", record.Status)
			}
			maps := &corev1.ConfigMap{}
			if err := w.c.Get(context.Background(), types.NamespacedName{Name: postfixVirtualMailboxMapsConfigMap, Namespace: postfixNamespace}, maps); err != nil {
				t.Fatal(err)
			}
			want := "demo.k.example OK\n" + refusal("alice@demo.k.example")
			if got := maps.Data[postfixVirtualMailboxDomainsKey]; got != want {
				t.Fatalf("once the person is gone, and before the mailbox is touched:\n got %q\nwant %q", got, want)
			}
			if got := maps.Data[postfixSenderAccessKey]; got != "demo.k.example OK\n" {
				t.Errorf("who may send changed: %q", got)
			}
			if got := maps.Data[postfixVirtualMailboxMapsKey]; got != "@demo.k.example demo.k.example/\n" {
				t.Errorf("the catch-all for everybody else changed: %q", got)
			}

			// Carried out: still refused, for as long as the record stands.
			w.now = w.now.Add(mailboxSignInSettle + time.Second)
			w.reconcile("demo-r1")
			jobs := w.jobs()
			if len(jobs) != 1 {
				t.Fatalf("%d Job(s)", len(jobs))
			}
			report := "result=archived\nbytes=1\nmessages=1\n"
			if choice == gentianov1alpha1.MailboxChoiceDelete {
				report = "result=deleted\nbytes=1\nmessages=1\n"
			}
			w.finish(&jobs[0], report, false)
			w.reconcile("demo-r1")
			if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != want {
				t.Fatalf("after the mailbox was %sd: %q", choice, got)
			}
		})
	}
}

// The address given to somebody new receives again, in whichever realm of
// the domain the new person is, switched off or not. And the record of a
// deleted mailbox is removed in time, which ends the refusal with it.
func TestARemovedAddressReceivesAgainOnceSomebodyHoldsIt(t *testing.T) {
	w := newMailboxWorld(t, selfhosted("demo"), mailRegistry(map[string]string{"demo": "demo.k.example"}),
		mailboxRecord("demo-r1", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	w.reconcile("demo-r1")
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; !strings.Contains(got, refusal("alice@demo.k.example")) {
		t.Fatalf("to begin with: %q", got)
	}
	w.people.set("demo", person("alice@demo.k.example", false))
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != "demo.k.example OK\n" {
		t.Errorf("with a new person at the address: %q", got)
	}
	w.people.set("demo")
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; !strings.Contains(got, refusal("alice@demo.k.example")) {
		t.Errorf("with that person gone again: %q", got)
	}

	// The cluster's own domain has two realms.
	user := selfhosted(gentianov1alpha1.SingleUserTenantName)
	w = newMailboxWorld(t, user, mailRegistry(map[string]string{user.Name: "k.example"}),
		mailboxRecord("u-r1", user.Name, "dana@k.example", gentianov1alpha1.MailboxChoiceArchive))
	w.tr.TenancyMode = gentianov1alpha1.TenancyModeSingle
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; !strings.Contains(got, refusal("dana@k.example")) {
		t.Fatalf("on the cluster's own domain: %q", got)
	}
	w.people.set("kernel", keycloakRealmUser{Username: "dana", Email: "dana@k.example", Enabled: true})
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; strings.Contains(got, "dana@") {
		t.Errorf("an address an administrator of the kernel realm holds is refused: %q", got)
	}

	// The record goes, and the refusal with it.
	w = newMailboxWorld(t, selfhosted("demo"), mailRegistry(map[string]string{"demo": "demo.k.example"}),
		mailboxRecord("demo-r1", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete))
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; !strings.Contains(got, refusal("alice@demo.k.example")) {
		t.Fatalf("to begin with: %q", got)
	}
	record := &gentianov1alpha1.MailboxRemoval{}
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: "demo-r1"}, record); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Delete(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != "demo.k.example OK\n" {
		t.Errorf("with the record gone: %q", got)
	}
}

// A refusal is permanent for the sender, so nothing is refused on a guess.
// While the people of a domain cannot be listed its addresses stay as they
// are: one refused before stays refused, and none is added.
func TestNothingAboutRemovedAddressesChangesWhileTheRealmCannotBeAsked(t *testing.T) {
	ctx := context.Background()
	w := newMailboxWorld(t, selfhosted("demo"), selfhosted("other"),
		mailRegistry(map[string]string{"demo": "demo.k.example", "other": "other.k.example"}),
		mailboxRecord("demo-r1", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive))
	want := "demo.k.example OK\nother.k.example OK\n" + refusal("alice@demo.k.example")
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != want {
		t.Fatalf("to begin with: %q", got)
	}

	w.people.down = true
	// Somebody was given the address meanwhile, and another person removed:
	// neither can be known.
	if err := w.c.Create(ctx, mailboxRecord("demo-r2", "demo", "bob@demo.k.example", gentianov1alpha1.MailboxChoiceDelete)); err != nil {
		t.Fatal(err)
	}
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != want {
		t.Errorf("with the identity provider down: %q", got)
	}

	w.people.down = false
	w.people.set("demo", person("alice@demo.k.example", true))
	want = "demo.k.example OK\nother.k.example OK\n" + refusal("bob@demo.k.example")
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != want {
		t.Errorf("once it answers again: %q", got)
	}
}

// What a record names is held to what its tenant has, as it is before a
// mailbox is touched: a record the operator refused, an address outside the
// tenant's own mail domain, a name that is no mailbox's, and the addresses
// every domain must accept refuse nothing.
func TestARecordRefusesNoAddressItMayNotName(t *testing.T) {
	refused := mailboxRecord("demo-refused", "demo", "frank@demo.k.example", gentianov1alpha1.MailboxChoiceDelete)
	refused.Status.Refused = true
	relay := planTenant("relay")
	relay.Spec.Mail = &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeTransportOnly}
	objects := []client.Object{
		selfhosted("demo"), selfhosted("other"), relay, refused,
		mailRegistry(map[string]string{"demo": "demo.k.example", "other": "other.k.example", "relay": "relay.k.example"}),
		mailboxRecord("demo-other", "demo", "alice@other.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-elsewhere", "demo", "alice@elsewhere.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-path", "demo", "a/b@demo.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-archive", "demo", ".archive@demo.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-space", "demo", "x y@demo.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-postmaster", "demo", "postmaster@demo.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-abuse", "demo", "Abuse@demo.k.example", gentianov1alpha1.MailboxChoiceArchive),
		mailboxRecord("demo-dmarc", "demo", "dmarc@demo.k.example", gentianov1alpha1.MailboxChoiceArchive),
		mailboxRecord("gone-r1", "gone", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("relay-r1", "relay", "alice@relay.k.example", gentianov1alpha1.MailboxChoiceDelete),
		// The one that may.
		mailboxRecord("demo-r1", "demo", "Erin@Demo.K.Example", gentianov1alpha1.MailboxChoiceDelete),
		mailboxRecord("demo-r2", "demo", "erin@demo.k.example", gentianov1alpha1.MailboxChoiceArchive),
	}
	w := newMailboxWorld(t, objects...)
	want := "demo.k.example OK\nother.k.example OK\nrelay.k.example OK\n" + refusal("erin@demo.k.example")
	if got := w.postfixMaps()[postfixVirtualMailboxDomainsKey]; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// With no record there is nothing to ask the identity provider, and the map
// is what it always was.
func TestWithNoRemovedPersonTheDomainsFileIsTheDomains(t *testing.T) {
	w := newMailboxWorld(t, selfhosted("demo"), mailRegistry(map[string]string{"demo": "demo.k.example"}))
	w.people.down = true
	maps := w.postfixMaps()
	if maps[postfixVirtualMailboxDomainsKey] != "demo.k.example OK\n" || maps[postfixVirtualMailboxDomainsKey] != maps[postfixSenderAccessKey] {
		t.Errorf("domains %q, senders %q", maps[postfixVirtualMailboxDomainsKey], maps[postfixSenderAccessKey])
	}
}

// Under strict the list drops the address with the person; where strict
// keeps the catch-all because the list cannot be trusted, the removed
// person's address is refused all the same.
func TestStrictKeepingTheCatchAllStillRefusesARemovedAddress(t *testing.T) {
	w := newMailboxWorld(t, selfhosted("demo"), mailRegistry(map[string]string{"demo": "demo.k.example"}),
		mailboxRecord("demo-r1", "demo", "alice@demo.k.example", gentianov1alpha1.MailboxChoiceArchive))
	w.tr.MailRecipientPolicy = mailRecipientStrict
	// A realm with nobody in the domain: strict keeps the catch-all.
	maps := w.postfixMaps()
	if maps[postfixVirtualMailboxMapsKey] != "@demo.k.example demo.k.example/\n" {
		t.Fatalf("strict did not keep the catch-all here: %q", maps[postfixVirtualMailboxMapsKey])
	}
	if !strings.Contains(maps[postfixVirtualMailboxDomainsKey], refusal("alice@demo.k.example")) {
		t.Errorf("the removed person's address is accepted: %q", maps[postfixVirtualMailboxDomainsKey])
	}
}
