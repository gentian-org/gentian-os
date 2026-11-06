/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta/metatest"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

// Every provisioning Job the operator builds, by the digest of the Job,
// held to testdata/provisioning-jobs.txt.
func TestProvisioningJobBuildersTranscript(t *testing.T) {
	t.Setenv("POSTGRES_PROVISIONER_IMAGE", "")
	t.Setenv("MARIADB_PROVISIONER_IMAGE", "")
	t.Setenv("REDIS_PROVISIONER_IMAGE", "")
	t.Setenv("SEAWEEDFS_IMAGE", "")
	t.Setenv("KEYCLOAK_PROVISIONER_IMAGE", "")
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	pack := oidcAppConfig{profileName: "wiki", clientID: "demo-wiki", redirectURIs: []string{"https://wiki.example/cb"},
		pack: &oidc.Pack{ScopeName: "wiki", Mappers: []string{"groups"}}}
	jobs := map[string]*batchv1.Job{
		"realm":                makeRealmJob(tenant, "demo"),
		"client":               makeClientJob(tenant, "demo", "wiki", "demo-wiki", []string{"https://wiki.example/cb"}, false),
		"client, with secret":  makeClientJob(tenant, "demo", "wiki", "demo-wiki", []string{"https://wiki.example/cb"}, true),
		"saml client":          makeSAMLClientJob(tenant, "demo", "wiki", "https://wiki.example", "https://wiki.example/acs"),
		"admin":                makeAdminJob(tenant, "demo", "admin"),
		"realm disable":        makeRealmDisableJob(tenant, "demo"),
		"realm delete":         makeRealmDeleteJob(tenant, "demo", "kernel"),
		"cache user":           makeRedisACLJob(tenant, "wiki", false),
		"cache user, password": makeRedisACLJob(tenant, "wiki", true),
		"groups":               makeGentianGroupsJob(tenant, "demo", `[{"name":"g"}]`),
		"postgres role":        makeRoleJob(tenant, "tenant-demo", "demo_wiki", "wiki", true, gentianov1alpha1.SchemaPreferenceAppSchema, true, 25),
		"mariadb setup":        makeMariaDBSetupJob(tenant, "shop", true, true, 25),
		"kernel job":           newKernelProvisioningJob("job", "ns", tenant, "wiki", corev1.Container{Name: "c", Image: "i"}),
		"kernel job, no app":   newKernelProvisioningJob("job", "ns", tenant, "", corev1.Container{Name: "c", Image: "i"}),
		"bucket":               makeS3BucketJob(tenant, "wiki", true, backup.ObjectStorageOptions{}),
		"app instance role":    makeAppInstanceRoleJob(),
		"tenant smtp":          makeTenantSMTPJob("demo", "demo", "mail.example", "Example"),
		"oidc pack":            makeOIDCPackJob(tenant, "demo", pack, true, "wiki-users"),
	}
	var out []string
	for name, job := range jobs {
		out = append(out, fmt.Sprintf("%s: %s", name, jobDigest(t, job)))
	}
	sort.Strings(out)
	holdToTranscript(t, *updateTranscripts, "provisioning-jobs", strings.Join(out, "\n")+"\n")
}

// What the operator reads of a Job's state, held to
// testdata/job-status.txt.
func TestJobStatusTranscript(t *testing.T) {
	holdToTranscript(t, *updateTranscripts, "job-status", metatest.Transcript(map[string]func(*batchv1.Job) string{
		"complete":  func(j *batchv1.Job) string { return fmt.Sprint(jobIsComplete(j)) },
		"failed":    func(j *batchv1.Job) string { return fmt.Sprint(jobIsFailed(j)) },
		"failed2":   func(j *batchv1.Job) string { return fmt.Sprint(jobFailed(j)) },
		"ended":     func(j *batchv1.Job) string { return fmt.Sprint(jobEnded(j)) },
		"suspended": func(j *batchv1.Job) string { return fmt.Sprint(jobHasCondition(j, batchv1.JobSuspended)) },
	}))
}
