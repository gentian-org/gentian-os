/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// guaranteePlan plans the wiki bundle of tenant "old", written by line
// version, into tenant.
func guaranteePlan(t *testing.T, version string, tenant *gentianov1alpha1.Tenant) *restorePlan {
	t.Helper()
	m := wikiManifest()
	m.OperatorVersion = version
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		"notes": chartProfile("notes", "1.0.0", "", false),
	}
	claims := map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}
	plan, err := planRestore(m, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, claims))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// The round trip the platform guarantees is a bundle of its own version line
// restored into the tenant it was taken of. Anything else goes ahead, and
// says what differs on a condition of its own and among the notes.
func TestARestoreOutsideTheRoundTripSaysWhatDiffers(t *testing.T) {
	same := planTenant("old", "wiki", "notes")
	other := planTenant("demo", "wiki", "notes")

	cases := []struct {
		name, version string
		tenant        *gentianov1alpha1.Tenant
		reason        string
		says          []string
	}{
		{"the round trip", backup.PlatformVersionLine, same, "", nil},
		{"a build of the same line", backup.PlatformVersionLine + ".3+abc", same, "", nil},
		{"no version recorded", "", same, outsideGuaranteeVersion, []string{"does not say which platform version wrote it"}},
		{"an older line", "0.4", same, outsideGuaranteeVersion, []string{"written by platform version 0.4", "is not converted"}},
		{"another name", backup.PlatformVersionLine, other, outsideGuaranteeName, []string{"of tenant old and is restored into demo"}},
		{"both", "0.4", other, outsideGuaranteeSeveral, []string{"version 0.4", "tenant old"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-" + tc.tenant.Name}}
			recordPlan(restore, guaranteePlan(t, tc.version, tc.tenant))
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
