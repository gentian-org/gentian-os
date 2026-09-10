/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
		Spec:       gentianov1alpha1.TenantSpec{Isolation: isolation},
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

	bare := ProfileStores(&gentianov1alpha1.AppProfile{})
	if bare.Database != "" || bare.S3 || bare.Redis {
		t.Errorf("profile without kernelRequirements yielded %+v, want zero", bare)
	}

	full := ProfileStores(&gentianov1alpha1.AppProfile{
		Spec: gentianov1alpha1.AppProfileSpec{
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{
					Engine: gentianov1alpha1.DatabaseEnginePostgreSQL,
				},
				Storage: &gentianov1alpha1.StorageRequirement{
					S3: &gentianov1alpha1.S3Requirement{},
				},
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineRedis,
				},
			},
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

func TestOwnedByOtherRelease(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		app         string
		wantOther   bool
	}{
		{"no helm annotation", nil, "nextcloud-base-ce", false},
		{"own release", map[string]string{"meta.helm.sh/release-name": "nextcloud-base-ce-release"}, "nextcloud-base-ce", false},
		{"sibling release", map[string]string{"meta.helm.sh/release-name": "openproject-ce-release"}, "nextcloud-base-ce", true},
	}
	for _, tc := range cases {
		if _, got := OwnedByOtherRelease(tc.annotations, tc.app); got != tc.wantOther {
			t.Errorf("%s: OwnedByOtherRelease = %v, want %v", tc.name, got, tc.wantOther)
		}
	}
}
