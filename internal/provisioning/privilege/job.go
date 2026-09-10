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

package privilege

import (
	"encoding/json"
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	// FingerprintAnnotation records which membership a Job was built for, so a
	// finished Job can be told apart from one that is stale.
	FingerprintAnnotation = "gentianos.io/app-privilege-fingerprint"

	membersKey       = "members.json"
	membersMountPath = "/etc/gentian/app-admins"
	scriptMountPath  = "/scripts"
)

// Member is the privileged-user record handed to an app's sync script. It is
// deliberately the small intersection every app can act on: apps match users by
// email or username, never by Keycloak's internal model.
type Member struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// SecretName is the per-app Secret holding the published member list.
func SecretName(app string) string { return app + "-app-admins" }

// JobName is the per-app sync Job.
func JobName(app string) string { return app + "-privilege-sync" }

// MembersJSON renders members as the JSON array the sync script reads. Sorted
// by id so identical membership always produces identical bytes — otherwise the
// Secret would churn on every reconcile purely from map/list ordering.
func MembersJSON(members []authz.KeycloakUser) ([]byte, error) {
	out := make([]Member, 0, len(members))
	for _, m := range members {
		if m.ID == "" {
			continue
		}
		out = append(out, Member{ID: m.ID, Username: m.Username, Email: m.Email})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return json.Marshal(out)
}

// MembersSecret publishes the member list into the tenant namespace for the
// sync Job to read. A Secret rather than a ConfigMap: it carries the email
// addresses of named people, which is not something to leave in a more
// casually-readable object.
func MembersSecret(tenant, app, namespace string, membersJSON []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(app),
			Namespace: namespace,
			Labels:    labels(tenant, app),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{membersKey: membersJSON},
	}
}

// SyncJob renders the app-supplied script as a Job. Everything app-specific
// comes from spec — this function knows only how to run a script with a member
// list attached.
func SyncJob(
	tenant, app, namespace, fingerprint string,
	role *gentianov1alpha1.PrivilegedRoleSpec,
	spec *gentianov1alpha1.ProvisioningJobSpec,
) *batchv1.Job {
	roleName := ""
	if role != nil {
		roleName = role.Name
	}
	backoff := int32(4)
	ttl := int32(600)

	env := []corev1.EnvVar{
		{Name: "GENTIAN_APP_ADMINS_FILE", Value: membersMountPath + "/" + membersKey},
		{Name: "GENTIAN_PRIVILEGED_ROLE", Value: roleName},
		{Name: "GENTIAN_TENANT", Value: tenant},
		{Name: "GENTIAN_APP", Value: app},
	}
	for _, e := range spec.Env {
		if e.Name == "" || e.SecretKeyRef.Name == "" || e.SecretKeyRef.Key == "" {
			continue
		}
		env = append(env, corev1.EnvVar{
			Name: e.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.SecretKeyRef.Name},
					Key:                  e.SecretKeyRef.Key,
				},
			},
		})
	}
	envFrom := make([]corev1.EnvFromSource, 0, len(spec.EnvFrom))
	for _, name := range spec.EnvFrom {
		if name == "" {
			continue
		}
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}

	podLabels := labels(tenant, app)
	// The app's own NetworkPolicies select on this label, so the Job needs it to
	// reach the workload it is provisioning.
	podLabels["gentianos.io/component"] = "privilege-sync"

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(app),
			Namespace:   namespace,
			Labels:      labels(tenant, app),
			Annotations: map[string]string{FingerprintAnnotation: fingerprint},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
					Annotations: map[string]string{
						FingerprintAnnotation: fingerprint,
						meta.AppLabel:         app,
						meta.TenantLabel:      tenant,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: spec.ServiceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "privilege-sync",
						Image:   spec.Image,
						Command: []string{"/bin/sh", scriptMountPath + "/run.sh"},
						Env:     env,
						EnvFrom: envFrom,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "scripts", MountPath: scriptMountPath, ReadOnly: true},
							{Name: "app-admins", MountPath: membersMountPath, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  SecretName(app),
									Items:       []corev1.KeyToPath{{Key: "run.sh", Path: "run.sh"}},
									DefaultMode: ptr(int32(0o555)),
								},
							},
						},
						{
							Name: "app-admins",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: SecretName(app),
									Items:      []corev1.KeyToPath{{Key: membersKey, Path: membersKey}},
								},
							},
						},
					},
				},
			},
		},
	}
}

// JobState is what a reconcile needs to know about an existing sync Job.
type JobState int

const (
	JobRunning JobState = iota
	JobSucceeded
	JobFailed
)

// StateOf reports whether a Job finished, and how.
func StateOf(job *batchv1.Job) JobState {
	if job.Status.Succeeded > 0 {
		return JobSucceeded
	}
	backoff := int32(0)
	if job.Spec.BackoffLimit != nil {
		backoff = *job.Spec.BackoffLimit
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return JobFailed
		}
	}
	if backoff > 0 && job.Status.Failed > backoff {
		return JobFailed
	}
	return JobRunning
}

// FailureMessage summarises why a Job failed, for the tenant condition.
func FailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Message != "" {
			return c.Message
		}
	}
	return fmt.Sprintf("%d attempt(s) failed", job.Status.Failed)
}

// labels must stay exactly the selector the uninstall purge sweeps with
// (applifecycle.purgeClusterArtifacts), or these objects outlive the app they
// belong to. Shared constants rather than literals so the two cannot drift.
func labels(tenant, app string) map[string]string {
	return map[string]string{
		meta.ManagedByLabel: meta.ManagedByValue,
		meta.TenantLabel:    tenant,
		meta.AppLabel:       app,
	}
}

func ptr[T any](v T) *T { return &v }
