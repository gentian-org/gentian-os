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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// A profile's post-restore commands are told where the bundle was written and
// where its data is put back, so that the app's profile can rewrite what the
// app cached: the tenant, the domain, the platform version and the cluster on
// both sides, and for the app its versions and each store's two names.
func TestPostRestoreCommandsAreToldWhereTheDataCameFromAndWhereItIs(t *testing.T) {
	m := wikiManifest()
	m.OperatorVersion, m.Domain, m.Cluster, m.KernelDomain = "0.4", "old.example.org", "zrh-1", "example.org"
	m.Apps[0].Stores = append(m.Apps[0].Stores, backup.ManifestStore{Kind: "s3", Name: "bad name;rm", Path: "s3/x.tar.gz"})
	tenant := planTenant("old", "wiki", "notes")
	profile := chartProfile("wiki", "2.1.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true)
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"wiki": profile, "notes": chartProfile("notes", "1.0.0", "", false)}
	claims := map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}
	plan, err := planRestore(m, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, claims))
	if err != nil {
		t.Fatal(err)
	}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-old"}}
	recordPlan(restore, plan)
	// The databases wiki made itself, as the restore's Job reported them.
	for i, a := range appStatus(&restore.Status.Apps, "wiki").Artefacts {
		if a.Kind == bundle.ArtefactPostgresOwned {
			appStatus(&restore.Status.Apps, "wiki").Artefacts[i].Databases = []string{"old_wiki_reports=old_wiki_reports", "bad name=old_wiki_bad"}
		}
	}

	tr := &TenantReconciler{KernelDomain: "example.net", TenancyMode: "multi", ClusterID: "gva-2"}
	env := restoreHookEnv(restore, tenant, "wiki", profile, tr)
	want := []string{
		"GENTIAN_RESTORE_APP=wiki",
		"GENTIAN_RESTORE_SOURCE_TENANT=old",
		"GENTIAN_RESTORE_TARGET_TENANT=old",
		"GENTIAN_RESTORE_SOURCE_DOMAIN=old.example.org",
		"GENTIAN_RESTORE_TARGET_DOMAIN=old.example.net",
		"GENTIAN_RESTORE_SOURCE_KERNEL_DOMAIN=example.org",
		"GENTIAN_RESTORE_TARGET_KERNEL_DOMAIN=example.net",
		"GENTIAN_RESTORE_SOURCE_PLATFORM_VERSION=0.4",
		"GENTIAN_RESTORE_TARGET_PLATFORM_VERSION=" + backup.PlatformVersionLine,
		"GENTIAN_RESTORE_SOURCE_CLUSTER=zrh-1",
		"GENTIAN_RESTORE_TARGET_CLUSTER=gva-2",
		"GENTIAN_RESTORE_SOURCE_CHART_VERSION=2.0.0",
		"GENTIAN_RESTORE_TARGET_CHART_VERSION=2.1.0",
		// The bundle's own names, and the stores here; a name that is not a
		// plain one is not passed on.
		"GENTIAN_RESTORE_DATABASES=old_wiki=old_wiki old_wiki_reports=old_wiki_reports =old_wiki_bad",
		"GENTIAN_RESTORE_ROLES=old_wiki=old_wiki",
		"GENTIAN_RESTORE_BUCKETS=old-wiki=old-wiki =old-wiki",
		"GENTIAN_RESTORE_VOLUMES=wiki-release-data=wiki-release-data",
	}
	if !slices.Equal(env, want) {
		t.Errorf("env =\n%s\nwant\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
	for _, kv := range env {
		if !strings.HasPrefix(kv, RestoreEnvPrefix) {
			t.Errorf("%s has not the prefix", kv)
		}
	}

	// Run in the app's pod by env(1), before the command; the verification
	// is run as it is declared.
	s := quiesceScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-0", Namespace: "tenant-old", Labels: map[string]string{"gentianos.io/app": "wiki"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	exec := &fakeExecer{}
	r := &TenantRestoreReconciler{Tenant: &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(pod).Build(), Exec: exec}}
	spec := &gentianov1alpha1.BackupSpec{Restore: &gentianov1alpha1.BackupRestore{
		Post: [][]string{{"wiki", "rewrite-urls"}}, Verify: []string{"wiki", "check"},
	}}
	if err := r.runRestoreHooks(context.Background(), "old", "wiki", spec, appStatus(&restore.Status.Apps, "wiki"), env); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("calls = %v", exec.calls)
	}
	post := exec.calls[0]
	if post[0] != "env" || !slices.Equal(post[1:len(env)+1], env) || !slices.Equal(post[len(env)+1:], []string{"wiki", "rewrite-urls"}) {
		t.Errorf("post-restore command = %v", post)
	}
	if !slices.Equal(exec.calls[1], []string{"wiki", "check"}) {
		t.Errorf("verification = %v", exec.calls[1])
	}
}

// Restored into a tenant of another name, the login an app's data names is
// the old tenant's: each engine's, by the name the platform gives it.
func TestPostRestoreCommandsAreToldTheOldAndNewDatabaseLogin(t *testing.T) {
	restore := &gentianov1alpha1.TenantRestore{Status: gentianov1alpha1.TenantRestoreStatus{
		SourceTenant: "old-co",
		Apps: []gentianov1alpha1.AppExportStatus{
			{Name: "shop-app", Artefacts: []gentianov1alpha1.BundleArtefact{
				{Kind: bundle.ArtefactMariaDB, Name: "old_co_shop_app", Target: "new_shop_app"},
				{Kind: bundle.ArtefactMariaDBOwned, Name: "old_co_shop_app", Target: "new_shop_app",
					Databases: []string{"old_co_shop_app_reports=new_shop_app_reports"}},
			}},
			{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{
				{Kind: bundle.ArtefactPostgres, Name: "old_co_wiki", Target: "new_wiki"},
				{Kind: bundle.ArtefactPostgresOwned, Name: "old-co_wiki", Target: "new_wiki"},
			}},
		},
	}}
	tenant := planTenant("new")
	value := func(env []string, name string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, RestoreEnvPrefix+name+"="); ok {
				return v
			}
		}
		t.Fatalf("no %s in %v", name, env)
		return ""
	}
	maria := restoreHookEnv(restore, tenant, "shop-app", nil, nil)
	if got := value(maria, "DATABASES"); got != "old_co_shop_app=new_shop_app old_co_shop_app_reports=new_shop_app_reports" {
		t.Errorf("DATABASES = %q", got)
	}
	if got := value(maria, "ROLES"); got != "old_co_shop_app=new_shop_app" {
		t.Errorf("ROLES = %q", got)
	}
	if got := value(restoreHookEnv(restore, tenant, "wiki", nil, nil), "ROLES"); got != "old-co_wiki=new_wiki" {
		t.Errorf("ROLES = %q", got)
	}
}

// The login is made new on every restore, so an app that made no databases
// of its own is told it all the same, once; and where the bundle does not
// say whose it was, the old login is left out rather than guessed.
func TestPostRestoreCommandsAreToldTheLoginOfAnAppWithNoDatabasesOfItsOwn(t *testing.T) {
	apps := []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactPostgres, Name: "old-co_wiki", Target: "new_wiki"},
		}},
		{Name: "shop-app", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactMariaDB, Name: "old_co_shop_app", Target: "new_shop_app"},
		}},
		{Name: "notes", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactPostgres, Name: "old_notes", Target: "new_notes"},
			{Kind: bundle.ArtefactPostgresOwned, Name: "old_notes_role", Target: "new_notes"},
		}},
		{Name: "files", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactVolume, Name: "files-data", Target: "files-data"},
		}},
	}
	tenant := planTenant("new")
	roles := func(sourceTenant, app string) string {
		restore := &gentianov1alpha1.TenantRestore{Status: gentianov1alpha1.TenantRestoreStatus{SourceTenant: sourceTenant, Apps: apps}}
		for _, kv := range restoreHookEnv(restore, tenant, app, nil, nil) {
			if v, ok := strings.CutPrefix(kv, RestoreEnvPrefix+"ROLES="); ok {
				return v
			}
		}
		t.Fatalf("no ROLES for %s", app)
		return ""
	}
	for _, c := range []struct{ source, app, want string }{
		{"old-co", "wiki", "old-co_wiki=new_wiki"},
		{"old-co", "shop-app", "old_co_shop_app=new_shop_app"},
		// The archive's role, and only once.
		{"old-co", "notes", "old_notes_role=new_notes"},
		{"", "notes", "old_notes_role=new_notes"},
		// No tenant recorded and no archive naming the role.
		{"", "wiki", ""},
		{"", "shop-app", ""},
		// No database at all.
		{"old-co", "files", ""},
	} {
		if got := roles(c.source, c.app); got != c.want {
			t.Errorf("source %q, %s: ROLES = %q, want %q", c.source, c.app, got, c.want)
		}
	}
}
