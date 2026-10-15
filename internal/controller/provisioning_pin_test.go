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
	t.Setenv("SEAWEEDFS_IMAGE", "")
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	for name, c := range map[string]struct {
		job  *batchv1.Job
		want string
	}{
		"postgres role": {
			makeRoleJob(tenant, "tenant-demo", "demo_wiki", "wiki", "pw", gentianov1alpha1.SchemaPreferenceAppSchema, true),
			"bb0e4ee798d4956cf3fe7481f70f3ac5a4dd5f396618563d9829873937718b8d",
		},
		"mariadb setup": {makeMariaDBSetupJob(tenant, "shop", "pw", true), "1e36c94ab69c16480a4b778b69e4526a7fcf98116ecfd16fd7a0a30e64b945ad"},
		// Both bucket digests moved once when the Jobs' image became the
		// S3 server's own, and again, deliberately, when the server became
		// SeaweedFS: the bucket and the app's identity are made with weed
		// shell in the server's image (kernel.DefaultSeaweedFSImage), the
		// master's address is the one Secret value the pod reads, and the
		// pod carries the label the store's NetworkPolicy admits it by.
		// The keyed one moved once more, deliberately, when the app's key
		// pair left the spec: APP_ACCESS_KEY and APP_SECRET_KEY are read
		// from the Secret of the Job's name, which the Job owns
		// (ensureBucketKeySecrets), and are written into no Job.
		"bucket":         {makeS3BucketJob(tenant, "wiki", true), "09420de1d58d46a42c70b0c6dd7acf7e6d9c9ef89980fa3f9d66f339d7543131"},
		"bucket, no key": {makeS3BucketJob(tenant, "wiki", false), "9ed306136ae01d209c6f12054064db4733ab99a3d140b375c6987cb24240a969"},
		"cache user":     {makeRedisACLJob(tenant, "wiki", "pw"), "980aa4ecd34b77778b2225cbe86860dfe74dd7bd6ee0e79493bc0cfd1235d57a"},
	} {
		if got := jobDigest(t, c.job); got != c.want {
			t.Errorf("%s: the Job provisioning creates has changed (digest %s, pinned %s)", name, got, c.want)
		}
	}
}
