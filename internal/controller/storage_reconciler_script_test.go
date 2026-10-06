/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
