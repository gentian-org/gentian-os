/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func jobDigest(t *testing.T, job *batchv1.Job) string {
	t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// The Jobs provisioning hands to the cluster, pinned whole.
//
// The admin environment blocks, the images and the object-storage script
// these Jobs carry were written out here and again in the inventory
// (internal/backup), which the delete and restore Jobs are built from. They
// are the inventory's now. A provisioning Job's pod template is immutable and
// its script is hashed to decide whether a finished Job is run again, so
// "the same thing, built from one place" has to mean byte for byte: each
// digest below was taken from the Jobs as they were built before the two
// were made one, and a change to any of them is a change to what every
// tenant is provisioned with -- which has to be meant, and is then made by
// replacing the digest.
func TestProvisioningJobsAreWhatTheyWere(t *testing.T) {
	t.Setenv("POSTGRES_PROVISIONER_IMAGE", "")
	t.Setenv("MARIADB_PROVISIONER_IMAGE", "")
	t.Setenv("REDIS_PROVISIONER_IMAGE", "")
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	for name, c := range map[string]struct {
		job  *batchv1.Job
		want string
	}{
		"postgres role": {
			makeRoleJob(tenant, "tenant-demo", "demo_wiki", "wiki", "pw", gentianov1alpha1.SchemaPreferenceAppSchema, true),
			"bb0e4ee798d4956cf3fe7481f70f3ac5a4dd5f396618563d9829873937718b8d",
		},
		"mariadb setup":  {makeMariaDBSetupJob(tenant, "shop", "pw", true), "44df95849dde8b2564e568068b715556d890992968e75dc57baa0a968a26ab35"},
		"bucket":         {makeS3BucketJob(tenant, "wiki", "AK", "SK"), "19414f541375de3bc183e4276409422ded25a5cb38d642b9bb7c1d0c00be311c"},
		"bucket, no key": {makeS3BucketJob(tenant, "wiki", "", ""), "06ee93e15655f2d2b6e8a6eac539c43007e0b4aca261b83f6f17b6616b6d65e6"},
		"cache user":     {makeRedisACLJob(tenant, "wiki", "pw"), "980aa4ecd34b77778b2225cbe86860dfe74dd7bd6ee0e79493bc0cfd1235d57a"},
	} {
		if got := jobDigest(t, c.job); got != c.want {
			t.Errorf("%s: the Job provisioning creates has changed (digest %s, pinned %s)", name, got, c.want)
		}
	}
}
