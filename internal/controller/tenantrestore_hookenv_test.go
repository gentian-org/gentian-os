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
		"GENTIAN_RESTORE_DATABASES=old_wiki=old_wiki",
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
