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

package privilege_test

import (
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/provisioning/privilege"
)

func testJobSpec() *gentianov1alpha1.ProvisioningJobSpec {
	return &gentianov1alpha1.ProvisioningJobSpec{
		Image:   "example/app:1.0",
		Script:  "#!/bin/sh\necho hi\n",
		EnvFrom: []string{"app-sensitive-values"},
	}
}

// Identical membership must produce identical bytes, or the published Secret
// churns on every reconcile purely from ordering and the fingerprint stops
// meaning anything.
func TestMembersJSON_IsStableRegardlessOfOrder(t *testing.T) {
	t.Parallel()
	a := []authz.KeycloakUser{
		{ID: "2", Username: "bob", Email: "bob@example.org"},
		{ID: "1", Username: "alice", Email: "alice@example.org"},
	}
	b := []authz.KeycloakUser{
		{ID: "1", Username: "alice", Email: "alice@example.org"},
		{ID: "2", Username: "bob", Email: "bob@example.org"},
	}
	ja, err := privilege.MembersJSON(a)
	if err != nil {
		t.Fatalf("MembersJSON: %v", err)
	}
	jb, err := privilege.MembersJSON(b)
	if err != nil {
		t.Fatalf("MembersJSON: %v", err)
	}
	if string(ja) != string(jb) {
		t.Fatalf("ordering leaked into output:\n%s\n%s", ja, jb)
	}

	var out []privilege.Member
	if err := json.Unmarshal(ja, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 2 || out[0].Username != "alice" {
		t.Fatalf("unexpected members: %+v", out)
	}
}

func TestMembersJSON_SkipsEntriesWithoutAnID(t *testing.T) {
	t.Parallel()
	j, err := privilege.MembersJSON([]authz.KeycloakUser{{Username: "ghost"}})
	if err != nil {
		t.Fatalf("MembersJSON: %v", err)
	}
	if string(j) != "[]" {
		t.Fatalf("expected empty array, got %s", j)
	}
}

// The whole point of this mechanism: everything app-specific arrives from the
// AppProfile. If the platform ever has to know an app by name to provision it,
// this is where that would first show up.
func TestSyncJob_TakesEverythingAppSpecificFromTheProfile(t *testing.T) {
	t.Parallel()
	role := &gentianov1alpha1.PrivilegedRoleSpec{Kind: gentianov1alpha1.PrivilegedRoleKindGroup, Name: "admin"}
	job := privilege.SyncJob("demo", "someapp-ce", "tenant-demo", "fp1", role, testJobSpec())

	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "example/app:1.0" {
		t.Fatalf("image not taken from profile: %q", c.Image)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "app-sensitive-values" {
		t.Fatalf("envFrom not taken from profile: %+v", c.EnvFrom)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["GENTIAN_PRIVILEGED_ROLE"] != "admin" {
		t.Errorf("role not passed through: %q", env["GENTIAN_PRIVILEGED_ROLE"])
	}
	if env["GENTIAN_TENANT"] != "demo" || env["GENTIAN_APP"] != "someapp-ce" {
		t.Errorf("identity not passed through: %+v", env)
	}
	if env["GENTIAN_APP_ADMINS_FILE"] == "" {
		t.Errorf("member list path not provided")
	}

	if got := job.Annotations[privilege.FingerprintAnnotation]; got != "fp1" {
		t.Errorf("fingerprint not recorded: %q", got)
	}
	// The app's NetworkPolicies select on this label; without it the Job cannot
	// reach the workload it is meant to provision.
	if got := job.Spec.Template.Labels["gentianos.io/app"]; got != "someapp-ce" {
		t.Errorf("pod not labelled for the app's network policy: %q", got)
	}
}

func TestStateOf(t *testing.T) {
	t.Parallel()
	backoff := int32(4)

	running := &batchv1.Job{Spec: batchv1.JobSpec{BackoffLimit: &backoff}}
	if privilege.StateOf(running) != privilege.JobRunning {
		t.Errorf("fresh job should be running")
	}

	succeeded := &batchv1.Job{Status: batchv1.JobStatus{Succeeded: 1}}
	if privilege.StateOf(succeeded) != privilege.JobSucceeded {
		t.Errorf("succeeded job misread")
	}

	failed := &batchv1.Job{Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded",
		}},
	}}
	if privilege.StateOf(failed) != privilege.JobFailed {
		t.Errorf("failed job misread")
	}
	if msg := privilege.FailureMessage(failed); msg != "BackoffLimitExceeded" {
		t.Errorf("failure message lost: %q", msg)
	}
}

func TestMembersSecret_CarriesTheMemberList(t *testing.T) {
	t.Parallel()
	s := privilege.MembersSecret("demo", "someapp-ce", "tenant-demo", []byte(`[]`))
	if s.Name != privilege.SecretName("someapp-ce") || s.Namespace != "tenant-demo" {
		t.Fatalf("unexpected secret identity: %s/%s", s.Namespace, s.Name)
	}
	if _, ok := s.Data["members.json"]; !ok {
		t.Fatalf("member list missing: %+v", s.Data)
	}
	var _ metav1.Object = s
}
