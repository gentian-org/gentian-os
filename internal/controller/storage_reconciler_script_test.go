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

package controller

import (
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Deleting a bucket also removes the user and policy made for it, found
// through the policy statement that names the bucket exactly -- a prefix
// match would take a sibling bucket's user with it.
func TestMinioDeleteScript_RemovesUserAndPolicy(t *testing.T) {
	script := minioDeleteScript("demo-files")
	for _, want := range []string{
		`mc rb --force "gentian/demo-files"`,
		`arn:aws:s3:::demo-files"`,
		`mc admin user rm gentian "${policy%-policy}"`,
		`mc admin policy rm gentian "${policy}"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("minioDeleteScript() missing %q\nscript:\n%s", want, script)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The backup bucket is deleted as its own unit, and the pseudo-app name the
// Job carries resolves to exactly the bucket exports write to.
func TestBackupBucketUnitNamesTheBackupBucket(t *testing.T) {
	for _, tenant := range []*gentianov1alpha1.Tenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "acme"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: gentianov1alpha1.TenantSpec{
			Isolation: &gentianov1alpha1.TenantIsolation{S3Prefix: "Corp_"}}},
	} {
		if got, want := s3BucketName(tenant, backupBucketUnit), backup.BackupBucket(tenant); got != want {
			t.Fatalf("backup bucket unit = %q, exports write to %q", got, want)
		}
	}
}
