/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func jobRunning(script string, env ...corev1.EnvVar) batchv1.Job {
	return batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Image: "postgres:17",
						Args:  []string{script},
						Env:   env,
					}},
				},
			},
		},
	}
}

// A Job's pod template is immutable, so a changed script only reaches the
// cluster if the Job is recreated — which requires noticing it changed.
func TestScriptHash_ChangesWithTheScript(t *testing.T) {
	t.Parallel()
	a := jobRunning("ALTER ROLE x WITH NOCREATEDB;")
	b := jobRunning("ALTER ROLE x WITH CREATEDB;")
	if scriptHash(&a) == scriptHash(&b) {
		t.Fatal("a changed script produced the same hash, so the Job would never be re-run")
	}
}

// ROLE_PW is regenerated on every reconcile when no seeder supplies it. Hashing
// env would make every Job permanently stale and delete them in a loop.
func TestScriptHash_IgnoresEnvChurn(t *testing.T) {
	t.Parallel()
	a := jobRunning("same script", corev1.EnvVar{Name: "ROLE_PW", Value: "first"})
	b := jobRunning("same script", corev1.EnvVar{Name: "ROLE_PW", Value: "second"})
	if scriptHash(&a) != scriptHash(&b) {
		t.Fatal("env churn changed the hash; provisioning Jobs would be deleted every reconcile")
	}
}

func TestStampScriptHashes_AnnotatesEveryJob(t *testing.T) {
	t.Parallel()
	jobs := []batchv1.Job{jobRunning("one"), jobRunning("two")}
	stampScriptHashes(jobs)
	for i, j := range jobs {
		if j.Annotations[provisioningScriptHashAnnotation] == "" {
			t.Fatalf("job %d not stamped", i)
		}
	}
	if jobs[0].Annotations[provisioningScriptHashAnnotation] ==
		jobs[1].Annotations[provisioningScriptHashAnnotation] {
		t.Fatal("different scripts stamped identically")
	}
}
