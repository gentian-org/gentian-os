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

// A Job is handed its inputs in env — the groups a realm is to have, whether
// its administrator needs a second factor. A changed input is a Job that has
// to run again, exactly like a changed script: no TTL re-runs it any more.
func TestScriptHash_ChangesWithAnInput(t *testing.T) {
	t.Parallel()
	a := jobRunning("same script", corev1.EnvVar{Name: "TENANT_ADMIN_REQUIRE_MFA", Value: "true"})
	b := jobRunning("same script", corev1.EnvVar{Name: "TENANT_ADMIN_REQUIRE_MFA", Value: "false"})
	if scriptHash(&a) == scriptHash(&b) {
		t.Fatal("a changed input produced the same hash, so the Job would never be re-run with it")
	}
}

// The hash of an unchanged Job is the same on every reconcile; a hash that
// moved by itself would retire every finished Job on every pass, which is the
// endless re-run this hash exists to end.
func TestScriptHash_IsStableForAnUnchangedJob(t *testing.T) {
	t.Parallel()
	a := jobRunning("same script", corev1.EnvVar{Name: "GENTIAN_GROUPS_JSON", Value: `[{"name":"g"}]`})
	b := jobRunning("same script", corev1.EnvVar{Name: "GENTIAN_GROUPS_JSON", Value: `[{"name":"g"}]`})
	if scriptHash(&a) != scriptHash(&b) {
		t.Fatal("the same Job hashed differently twice")
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
