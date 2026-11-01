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

	"github.com/gentian-org/gentian-os/internal/backup"
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
		// The postgres role, the MariaDB user and the cache user moved
		// once, deliberately, when the app's password left the spec:
		// ROLE_PW, DB_PASS and REDIS_USER_PASSWORD are read from the
		// Secret of the Job's name, which the Job owns
		// (ensureProvisioningJobSecrets), and are written into no Job.
		// Nothing else in these Jobs changed.
		"postgres role": {
			makeRoleJob(tenant, "tenant-demo", "demo_wiki", "wiki", true, gentianov1alpha1.SchemaPreferenceAppSchema, true),
			"6d295abc70894a261541959e5db6373b3066803534fa03dc961722d600c122fd", // moved deliberately: psql refuses an unencrypted connection (PGSSLMODE)
		},
		"mariadb setup": {makeMariaDBSetupJob(tenant, "shop", true, true), "14691be0177479f93914a328cb59573120d722f67ee2d773388b791be7b10d5e"},
		// Both bucket digests moved once when the Jobs' image became the
		// S3 server's own, and again, deliberately, when the server became
		// SeaweedFS: the bucket and the app's identity are made with weed
		// shell in the server's image (kernel.DefaultSeaweedFSImage), the
		// master's address is the one Secret value the pod reads, and the
		// pod carries the label the store's NetworkPolicy admits it by.
		// The keyed one moved once more, deliberately, when the app's key
		// pair left the spec: APP_ACCESS_KEY and APP_SECRET_KEY are read
		// from the Secret of the Job's name, which the Job owns
		// (ensureProvisioningJobSecrets), and are written into no Job.
		// Both moved again, deliberately, when the store's administration
		// became mutual TLS: the pod mounts the client certificate
		// (backup.ObjectStoreClientTLSSecret), weed shell reads its
		// [grpc.client] settings from WEED_GRPC_*, and the filer's signing
		// key, which s3.configure needs a token of, is read from the
		// store's signing Secret.
		"bucket":         {makeS3BucketJob(tenant, "wiki", true, backup.ObjectStorageOptions{}), "b8b6bda18925bf9da9324ce60f1e574280306f454068d0a337d44573311c28b2"},
		"bucket, no key": {makeS3BucketJob(tenant, "wiki", false, backup.ObjectStorageOptions{}), "56ebf6847888baa1559a5429f977c4c1bb81c6fbd8267282480dcd42419cda2a"},
		// Moved deliberately with the move to Valkey: the user is held to its
		// prefix and saved, so every cache user is made again on the new server.
		"cache user": {makeRedisACLJob(tenant, "wiki", true), "194ee456921cb6d90d2e116eb26740c521f26adcd7a051c3142799baa5acf304"},
	} {
		if got := jobDigest(t, c.job); got != c.want {
			t.Errorf("%s: the Job provisioning creates has changed (digest %s, pinned %s)", name, got, c.want)
		}
	}
}
