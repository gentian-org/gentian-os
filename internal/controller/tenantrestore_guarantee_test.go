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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// guaranteePlan plans the wiki bundle of tenant "demo", written by line
// version on domain, into tenant on old.example.org.
func guaranteePlan(t *testing.T, version, domain string, tenant *gentianov1alpha1.Tenant) *restorePlan {
	t.Helper()
	m := wikiManifest()
	m.OperatorVersion, m.Domain = version, domain
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		"notes": chartProfile("notes", "1.0.0", "", false),
	}
	claims := map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}
	target := targetOf(tenant)
	target.domain = "old.example.org"
	plan, err := planRestore(m, tenant, nil, false, target, liveFrom(tenant, profiles, claims))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// The round trip the platform guarantees is a bundle of its own version line
// restored into the tenant it was taken of, on its domain. Anything else
// goes ahead, and says what differs on a condition of its own and among the
// notes.
func TestARestoreOutsideTheRoundTripSaysWhatDiffers(t *testing.T) {
	tenant := planTenant("demo", "wiki", "notes")
	here := "old.example.org"

	cases := []struct {
		name, version, domain string
		reason                string
		says                  []string
	}{
		{"the round trip", backup.PlatformVersionLine, here, "", nil},
		{"a build of the same line", backup.PlatformVersionLine + ".3+abc", here, "", nil},
		{"no version recorded", "", here, outsideGuaranteeVersion, []string{"does not say which platform version wrote it"}},
		{"an older line", "0.4", here, outsideGuaranteeVersion, []string{"written by platform version 0.4", "is not converted"}},
		{"both", "0.4", "old.example.net", outsideGuaranteeSeveral, []string{"version 0.4", "taken on the domain old.example.net"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-" + tenant.Name}}
			recordPlan(restore, guaranteePlan(t, tc.version, tc.domain, tenant))
			cond := meta.FindStatusCondition(restore.Status.Conditions, conditionOutsideGuarantee)
			if tc.reason == "" {
				if cond != nil {
					t.Fatalf("the round trip is said to be outside it: %+v", cond)
				}
				for _, n := range restore.Status.Notes {
					if strings.Contains(n, "outside what the platform guarantees") {
						t.Errorf("note: %s", n)
					}
				}
				return
			}
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.reason {
				t.Fatalf("condition = %+v, want True %s", cond, tc.reason)
			}
			for _, want := range tc.says {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("the condition does not say %q: %s", want, cond.Message)
				}
			}
			if !slices.Contains(restore.Status.Notes, cond.Message) {
				t.Errorf("the notes do not repeat it: %v", restore.Status.Notes)
			}
		})
	}
}

// A tenant may move to another domain with its data; the restore goes ahead
// and says that the data names the old domain, and, where the old one was a
// custom domain, that its binding has to be declared again.
func TestARestoreOnAnotherDomainSaysSoAndThatACustomDomainIsDeclaredAgain(t *testing.T) {
	plan := func(domain, custom, here string, tenant *gentianov1alpha1.Tenant) *gentianov1alpha1.TenantRestore {
		t.Helper()
		m := wikiManifest()
		m.Tenant, m.OperatorVersion, m.Domain, m.CustomDomain = tenant.Name, backup.PlatformVersionLine, domain, custom
		profiles := map[string]*gentianov1alpha1.ComponentProfile{
			"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
			"notes": chartProfile("notes", "1.0.0", "", false),
		}
		claims := map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}
		target := targetOf(tenant)
		target.domain = here
		p, err := planRestore(m, tenant, nil, false, target, liveFrom(tenant, profiles, claims))
		if err != nil {
			t.Fatal(err)
		}
		restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-" + tenant.Name}}
		recordPlan(restore, p)
		return restore
	}
	tenant := planTenant("acme", "wiki", "notes")

	// The same domain: the round trip.
	if r := plan("acme.example.org", "", "acme.example.org", tenant); meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee) != nil {
		t.Errorf("the same domain is said to differ: %+v", r.Status.Conditions)
	}
	// Another cluster's domain.
	r := plan("acme.example.org", "", "acme.example.net", tenant)
	cond := meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee)
	if cond == nil || cond.Reason != outsideGuaranteeDomain || !strings.Contains(cond.Message, "taken on the domain acme.example.org and the tenant is on acme.example.net") ||
		strings.Contains(cond.Message, "TenantDomain") {
		t.Fatalf("condition = %+v", cond)
	}
	// A custom domain, not bound here.
	r = plan("acme.ch", "acme.ch", "acme.example.net", tenant)
	cond = meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee)
	if cond == nil || !strings.Contains(cond.Message, "declare it again (kubectl gentian tenants domain acme acme.ch)") {
		t.Fatalf("condition = %+v", cond)
	}
	if r.Status.Source == nil || r.Status.Source.CustomDomain != "acme.ch" || r.Status.Source.Domain != "acme.ch" {
		t.Errorf("source = %+v", r.Status.Source)
	}
	// Bound here already: the round trip again.
	bound := planTenant("acme", "wiki", "notes")
	bound.Status.Domain = "acme.ch"
	if r := plan("acme.ch", "acme.ch", "acme.ch", bound); meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee) != nil {
		t.Errorf("a custom domain bound here again is said to differ: %+v", r.Status.Conditions)
	}
	// A bundle that records no domain says nothing of one, nor one that
	// records something that is not a domain: a bundle comes from anywhere,
	// and what it says is said back only where it is a plain name.
	for _, domain := range []string{"", "bad domain; " + strings.Repeat("x", 300)} {
		if r := plan(domain, domain, "acme.example.net", tenant); meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee) != nil ||
			r.Status.Source != nil && (r.Status.Source.Domain != "" || r.Status.Source.CustomDomain != "") {
			t.Errorf("a bundle with the domain %q is said to differ: %+v %+v", domain, r.Status.Conditions, r.Status.Source)
		}
	}
}

// An app whose profile binds its data to the domain
// (spec.backup.restore.domainBound) is restored only onto the domain the
// bundle was taken on. Onto another its data is left out, named with the
// reason, and the move says so; the other apps are restored as ever. A
// bundle that does not say its domain is restored as onto the same one, and
// the result says it could not tell.
func TestAnAppBoundToItsDomainIsNotRestoredOntoAnother(t *testing.T) {
	tenant := planTenant("acme", "wiki", "chat")
	chat := chartProfile("chat", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false)
	chat.Spec.Backup = &gentianov1alpha1.BackupSpec{Restore: &gentianov1alpha1.BackupRestore{DomainBound: true}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		"chat": chat,
	}
	claims := map[string][]string{"wiki": {"wiki-release-data"}}
	manifest := func(domain string) *backup.Manifest {
		m := wikiManifest()
		m.Tenant, m.OperatorVersion, m.Domain = "acme", backup.PlatformVersionLine, domain
		m.Apps = []backup.ManifestApp{m.Apps[0], {Name: "chat", ChartVersion: "1.0.0", Stores: []backup.ManifestStore{
			{Kind: bundle.ArtefactPostgres, Name: "acme_chat", Path: "postgres/acme_chat.pgc"},
		}}}
		return m
	}
	plan := func(domain, here string, apps ...string) (*gentianov1alpha1.TenantRestore, error) {
		t.Helper()
		target := targetOf(tenant)
		target.domain = here
		p, err := planRestore(manifest(domain), tenant, apps, false, target, liveFrom(tenant, profiles, claims))
		if err != nil {
			return nil, err
		}
		restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-acme"}}
		recordPlan(restore, p)
		return restore, nil
	}
	restored := func(r *gentianov1alpha1.TenantRestore) []string { return plannedApps(r) }

	// Onto another domain: chat is left out, and said to be.
	r, err := plan("acme.example.org", "acme.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(r); !slices.Equal(got, []string{"wiki"}) {
		t.Errorf("restored %v, want wiki only", got)
	}
	if len(r.Status.NotRestored) != 1 || r.Status.NotRestored[0].App != "chat" ||
		!strings.Contains(r.Status.NotRestored[0].Reason, "spec.backup.restore.domainBound") ||
		!strings.Contains(r.Status.NotRestored[0].Reason, "taken on acme.example.org while the tenant is on acme.example.net here: its data was NOT restored") {
		t.Errorf("notRestored = %+v", r.Status.NotRestored)
	}
	cond := meta.FindStatusCondition(r.Status.Conditions, conditionOutsideGuarantee)
	if cond == nil || cond.Reason != outsideGuaranteeDomain || !strings.Contains(cond.Message, "The data of chat, which names the old domain") {
		t.Errorf("condition = %+v", cond)
	}
	// Asked for by name onto another domain: refused before anything is
	// changed, as any app asked for that cannot be restored.
	if _, err := plan("acme.example.org", "acme.example.net", "chat"); err == nil || !strings.Contains(err.Error(), "domainBound") {
		t.Errorf("chat asked for onto another domain: %v", err)
	}

	// The same domain, in another case: restored as any app, nothing said.
	r, err = plan("ACME.example.org", "acme.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(r); !slices.Equal(got, []string{"wiki", "chat"}) || len(r.Status.NotRestored) != 0 {
		t.Errorf("restored %v, not restored %+v", got, r.Status.NotRestored)
	}
	for _, n := range r.Status.Notes {
		if strings.Contains(n, "domainBound") {
			t.Errorf("note on the same domain: %s", n)
		}
	}

	// A bundle that does not say its domain: restored, and said.
	r, err = plan("", "acme.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(r); !slices.Equal(got, []string{"wiki", "chat"}) {
		t.Errorf("restored %v", got)
	}
	if !slices.Contains(r.Status.Notes, "chat: "+domainBoundUnknownNote) {
		t.Errorf("the notes do not say the domain could not be told: %v", r.Status.Notes)
	}
}

// The domain a bundle was taken on is what the platform computed there:
// recorded, else the custom domain, else <tenant>.<kernel domain> -- except
// for a tenant that may have been the one user tenant of a single-tenancy
// cluster, whose domain was the kernel domain itself.
func TestTheDomainABundleWasTakenOn(t *testing.T) {
	for _, tc := range []struct {
		source *gentianov1alpha1.RestoreSource
		tenant string
		want   string
	}{
		{nil, "acme", ""},
		{&gentianov1alpha1.RestoreSource{Domain: "acme.k.example", CustomDomain: "acme.ch", KernelDomain: "k.example"}, "acme", "acme.k.example"},
		{&gentianov1alpha1.RestoreSource{CustomDomain: "acme.ch", KernelDomain: "k.example"}, "acme", "acme.ch"},
		{&gentianov1alpha1.RestoreSource{KernelDomain: "k.example"}, "acme", "acme.k.example"},
		{&gentianov1alpha1.RestoreSource{KernelDomain: "k.example"}, gentianov1alpha1.SingleUserTenantName, ""},
		{&gentianov1alpha1.RestoreSource{KernelDomain: "k.example"}, "", ""},
		{&gentianov1alpha1.RestoreSource{KernelDomain: "k.example"}, "bad name; x", ""},
		{&gentianov1alpha1.RestoreSource{PlatformVersion: "0.5"}, "acme", ""},
	} {
		if got := bundleDomain(tc.source, tc.tenant); got != tc.want {
			t.Errorf("bundleDomain(%+v, %q) = %q, want %q", tc.source, tc.tenant, got, tc.want)
		}
	}
}

// A realm restore says what becomes of the people's addresses: moved to the
// tenant's mail domain here, or, from a bundle that does not say its own,
// left as they are.
func TestARealmRestoreSaysWhatBecomesOfThePeoplesAddresses(t *testing.T) {
	tenant := planTenant("acme", "wiki", "notes")
	notes := func(domain, here string) string {
		t.Helper()
		m := wikiManifest()
		m.Tenant, m.Domain = "acme", domain
		target := targetOf(tenant)
		target.domain = here
		p, err := planRestore(m, tenant, nil, false, target, liveFrom(tenant, map[string]*gentianov1alpha1.ComponentProfile{
			"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true), "notes": chartProfile("notes", "1.0.0", "", false),
		}, map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(p.notes, "\n")
	}
	if n := notes("acme.example.org", "acme.example.net"); !strings.Contains(n, "addresses at acme.example.org were changed to the same name at acme.example.net") {
		t.Errorf("a move does not say the addresses moved:\n%s", n)
	}
	if n := notes("", "acme.example.net"); !strings.Contains(n, "an address at the old domain was NOT changed to acme.example.net") {
		t.Errorf("an unknown domain does not say the addresses were left:\n%s", n)
	}
	if n := notes("acme.example.org", "acme.example.org"); strings.Contains(n, "address") {
		t.Errorf("the same domain says something of addresses:\n%s", n)
	}
	// And of a sign-in client the tenant made: made at the new addresses, or
	// at the bundle's where its domain is not known.
	if n := notes("acme.example.org", "acme.example.net"); !strings.Contains(n, "sign-in client the tenant made that is not in the realm here was made with its addresses at acme.example.org moved to acme.example.net") {
		t.Errorf("a move does not say where the tenant's clients are made:\n%s", n)
	}
	if n := notes("", "acme.example.net"); !strings.Contains(n, "a sign-in client the tenant made that is not in the realm here was made at the addresses the bundle recorded") {
		t.Errorf("an unknown domain does not say the clients' addresses were left:\n%s", n)
	}
	if n := clientAddressNote("acme.old.example", "acme.new.example", "old.example", "new.example"); !strings.Contains(n, "and those at old.example to new.example") {
		t.Errorf("the cluster's domain is not named: %s", n)
	}
}

// The clients the platform makes for apps are named by the catalogue: each
// profile's OIDC clientId and SAML entity id, so a realm import knows them in
// a bundle taken before the platform marked its clients.
func TestTheCatalogueNamesThePlatformsAppClients(t *testing.T) {
	oidcProfile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "wiki"}}
	oidcProfile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "gentian-xwiki"}}}}
	samlProfile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "sso"}}
	samlProfile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{SAML: &gentianov1alpha1.SAMLClientSpec{EntityID: "urn:acme:sp", ACSURL: "x"}}}}
	bare := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &TenantRestoreReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(oidcProfile, samlProfile, bare).Build()}
	got, err := r.catalogueClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"gentian-xwiki", "urn:acme:sp"}) {
		t.Errorf("the catalog names %v", got)
	}
}
