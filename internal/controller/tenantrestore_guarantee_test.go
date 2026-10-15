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
