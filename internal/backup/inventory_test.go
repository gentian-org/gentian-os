/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenant(name string, isolation *gentianov1alpha1.TenantIsolation) *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant", Isolation: isolation},
	}
}

// The database replaces hyphens and the role does not. Conflating them is what
// left a login role behind on every purge, so the divergence is pinned here
// rather than left to be rediscovered.
func TestPostgresRoleIsNotSpelledLikeTheDatabase(t *testing.T) {
	tn := tenant("demo", nil)

	if got, want := DatabaseName(tn, "docmost-ce"), "demo_docmost_ce"; got != want {
		t.Errorf("DatabaseName = %q, want %q", got, want)
	}
	if got, want := PostgresRole("demo", "docmost-ce"), "demo_docmost-ce"; got != want {
		t.Errorf("PostgresRole = %q, want %q", got, want)
	}
	if DatabaseName(tn, "docmost-ce") == PostgresRole("demo", "docmost-ce") {
		t.Error("database and role names must not converge")
	}
}

func TestNamesHonourIsolationPrefixes(t *testing.T) {
	tn := tenant("demo", &gentianov1alpha1.TenantIsolation{
		DatabasePrefix: "corp_",
		S3Prefix:       "corp-",
	})

	if got, want := DatabaseName(tn, "nextcloud-base-ce"), "corp_nextcloud_base_ce"; got != want {
		t.Errorf("DatabaseName = %q, want %q", got, want)
	}
	if got, want := S3Bucket(tn, "nextcloud-base-ce"), "corp-nextcloud-base-ce"; got != want {
		t.Errorf("S3Bucket = %q, want %q", got, want)
	}

	plain := tenant("demo", nil)
	if got, want := DatabaseName(plain, "app"), "demo_app"; got != want {
		t.Errorf("DatabaseName without prefix = %q, want %q", got, want)
	}
	if got, want := S3Bucket(plain, "app"), "demo-app"; got != want {
		t.Errorf("S3Bucket without prefix = %q, want %q", got, want)
	}
}

func TestS3BucketRejectsIllegalCharacters(t *testing.T) {
	tn := tenant("Demo_Corp", nil)
	if got, want := S3Bucket(tn, "My_App"), "demo-corp-my-app"; got != want {
		t.Errorf("S3Bucket = %q, want %q", got, want)
	}
}

// The backup bucket must never collide with an app bucket, or an export would
// eventually try to capture its own previous output.
func TestBackupBucketCannotCollideWithAnAppBucket(t *testing.T) {
	tn := tenant("demo", nil)
	backupBucket := BackupBucket(tn)

	if got, want := backupBucket, "demo-gentian-backup"; got != want {
		t.Errorf("BackupBucket = %q, want %q", got, want)
	}
	// The only way an app bucket could match is a profile literally named
	// "gentian-backup"; that name is reserved by convention, and this asserts
	// the shape callers rely on rather than the reservation itself.
	for _, app := range []string{"nextcloud-base-ce", "backup", "gentian"} {
		if S3Bucket(tn, app) == backupBucket {
			t.Errorf("app %q produces the backup bucket name %q", app, backupBucket)
		}
	}

	prefixed := tenant("demo", &gentianov1alpha1.TenantIsolation{S3Prefix: "corp-"})
	if got, want := BackupBucket(prefixed), "corp-gentian-backup"; got != want {
		t.Errorf("BackupBucket with prefix = %q, want %q", got, want)
	}
}

func TestProfileStoresReadsOnlyKernelRequirements(t *testing.T) {
	none := ProfileStores(nil)
	if none.Database != "" || none.S3 || none.Redis {
		t.Errorf("nil profile yielded stores %+v, want zero", none)
	}

	bare := ProfileStores(&gentianov1alpha1.ComponentProfile{})
	if bare.Database != "" || bare.S3 || bare.Redis {
		t.Errorf("profile without kernelRequirements yielded %+v, want zero", bare)
	}

	full := ProfileStores(&gentianov1alpha1.ComponentProfile{
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{
					Engine: gentianov1alpha1.DatabaseEnginePostgreSQL,
				},
				Storage: &gentianov1alpha1.StorageRequirement{
					S3: &gentianov1alpha1.S3Requirement{},
				},
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineRedis,
				},
			}},
		},
	})
	if full.Database != gentianov1alpha1.DatabaseEnginePostgreSQL {
		t.Errorf("Database = %q", full.Database)
	}
	if !full.S3 || !full.Redis {
		t.Errorf("stores = %+v, want S3 and Redis set", full)
	}
}

func TestPVCBelongsToApp(t *testing.T) {
	pvc := func(name string, labels map[string]string) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		}
	}

	cases := []struct {
		name   string
		claim  corev1.PersistentVolumeClaim
		app    string
		family string
		want   bool
	}{
		{"explicit app label", pvc("data", map[string]string{"gentianos.io/app": "nextcloud-base-ce"}), "nextcloud-base-ce", "", true},
		{"instance prefix", pvc("data", map[string]string{"app.kubernetes.io/instance": "nextcloud-base-ce-abc"}), "nextcloud-base-ce", "", true},
		{"family name label", pvc("data", map[string]string{"app.kubernetes.io/name": "nextcloud"}), "nextcloud-base-ce", "nextcloud", true},
		{"name substring", pvc("nextcloud-base-ce-data", nil), "nextcloud-base-ce", "", true},
		{"unrelated claim", pvc("openproject-data", nil), "nextcloud-base-ce", "nextcloud", false},
	}
	for _, tc := range cases {
		if got := PVCBelongsToApp(tc.claim, tc.app, tc.family); got != tc.want {
			t.Errorf("%s: PVCBelongsToApp = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A release is named after its app, with nothing generated in the name: that
// is what lets a volume claim kept by an uninstall be recognised, and taken
// over, by the next install. Which releases are an app's is exact when its
// extensions are known, and only then.
func TestAnAppsReleasesAreNamedAfterIt(t *testing.T) {
	if got := AppRelease("odoo-base-ce"); got != "odoo-base-ce-release" {
		t.Errorf("AppRelease = %q", got)
	}
	if got := ExtensionRelease("odoo-base-ce", "mcp"); got != "odoo-base-ce-mcp-release" {
		t.Errorf("ExtensionRelease = %q", got)
	}
	if got := DirectRelease("demo", "desktop"); got != "tenant-demo-desktop" {
		t.Errorf("DirectRelease = %q", got)
	}
	cases := []struct {
		release    string
		extensions []string
		known      bool
		want       bool
	}{
		{"wiki-release", nil, true, true},
		{"tenant-demo-wiki", nil, true, true},
		{"wiki-mcp-release", []string{"mcp"}, true, true},
		// Another app whose name begins with this one's.
		{"wiki-pro-release", []string{"mcp"}, true, false},
		// With the extensions unknown the same release counts, so that an
		// app is never taken to be gone while something that may be its
		// own is still running.
		{"wiki-pro-release", nil, false, true},
		{"drive-release", nil, false, false},
		{"tenant-other-wiki", nil, false, false},
	}
	for _, c := range cases {
		if got := IsAppRelease(c.release, "demo", "wiki", c.extensions, c.known); got != c.want {
			t.Errorf("IsAppRelease(%q, extensions %v known=%v) = %v, want %v", c.release, c.extensions, c.known, got, c.want)
		}
	}
	if !IsPlatformStore(DesktopStore) || IsPlatformStore("wiki") {
		t.Error("the desktop's store is the platform's, and an app's is not")
	}
}
