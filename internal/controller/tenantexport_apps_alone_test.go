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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// appsAlone is an export of these apps without the tenant's own.
func appsAlone(name string, minute int, apps ...string) *gentianov1alpha1.TenantExport {
	no := false
	return &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-demo",
			CreationTimestamp: metav1.Time{Time: time.Date(2026, 11, 20, 3, minute, 0, 0, time.UTC)}},
		Spec: gentianov1alpha1.TenantExportSpec{Apps: apps, TenantCore: &no},
	}
}

// whole is an export that captures the tenant's own, of these apps or all.
func whole(name string, minute int, apps ...string) *gentianov1alpha1.TenantExport {
	e := appsAlone(name, minute, apps...)
	e.Spec.TenantCore = nil
	return e
}

// An export of apps alone holds those apps and nothing else: it waits only
// for an older export that captures one of them or the tenant's own, and an
// export of the tenant's own waits for every older one.
func TestAnExportOfAppsAloneHoldsOnlyThoseApps(t *testing.T) {
	scheme := deleteGapsScheme()
	for _, tc := range []struct {
		name   string
		older  *gentianov1alpha1.TenantExport
		export *gentianov1alpha1.TenantExport
		waits  bool
	}{
		{"apps alone, other apps", appsAlone("a", 0, "wiki"), appsAlone("b", 1, "shop"), false},
		{"apps alone, the same app", appsAlone("a", 0, "wiki", "chat"), appsAlone("b", 1, "chat"), true},
		{"apps alone after the tenant's own", whole("a", 0, "wiki"), appsAlone("b", 1, "shop"), true},
		{"the tenant's own after apps alone", appsAlone("a", 0, "wiki"), whole("b", 1), true},
		{"the newer never waits for the older's sake", appsAlone("a", 1, "wiki"), whole("b", 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.older, tc.export).Build()
			r := &TenantExportReconciler{Client: c, Scheme: scheme}
			holder, waits, err := r.blockedByAnotherExport(context.Background(), tc.export)
			if err != nil {
				t.Fatal(err)
			}
			if waits != tc.waits || (waits && holder != tc.older.Name) {
				t.Fatalf("waits %v for %q, want %v", waits, holder, tc.waits)
			}
		})
	}
}

// tenantCore: false is honored only with apps named: an export of the whole
// tenant always captures the tenant's own.
func TestTenantCoreFalseNeedsApps(t *testing.T) {
	if e := appsAlone("a", 0); !e.CapturesTenantCore() {
		t.Fatal("an export naming no app left the tenant's own out")
	}
	if e := appsAlone("a", 0, "wiki"); e.CapturesTenantCore() {
		t.Fatal("an export of wiki alone captures the tenant's own")
	}
}

// The bundle of apps alone records no realm, desktop database or mailboxes,
// says so, and restores the apps and nothing of the tenant's own.
func TestTheBundleOfAppsAloneHoldsNoneOfTheTenantsOwn(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "wiki", "shop")
	export := appsAlone("move-wiki", 0, "wiki")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, export).Build()
	r := &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: &TenantReconciler{Client: c, Scheme: scheme}}

	notIncluded, err := r.notIncluded(context.Background(), tenant, export)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(notIncluded, "; "); !strings.Contains(joined, "the tenant's own") || !strings.Contains(joined, "shop") {
		t.Fatalf("notIncluded = %q, want the tenant's own and the app not asked for", joined)
	}

	export.Status.Apps = []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", ChartVersion: "1.0.0", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"}}},
		{Name: backupTenantComponent},
	}
	m := r.buildManifest(export, tenant)
	if m.Identity != nil || m.Shell != nil || m.Mailboxes != nil {
		t.Fatalf("identity %+v, shell %+v, mailboxes %+v: the bundle holds none of them", m.Identity, m.Shell, m.Mailboxes)
	}
	if len(m.Apps) != 1 || m.Apps[0].Name != "wiki" {
		t.Fatalf("apps = %+v", m.Apps)
	}
	plan, err := planRestore(m, tenant, nil, false, targetOf(tenant), liveFrom(tenant,
		map[string]*gentianov1alpha1.ComponentProfile{"wiki": chartProfile("wiki", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false)},
		map[string][]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.apps) != 1 || len(plan.tenantWide) != 0 {
		t.Fatalf("a restore of it puts back apps %+v and %+v of the tenant's own", plan.apps, plan.tenantWide)
	}
}
