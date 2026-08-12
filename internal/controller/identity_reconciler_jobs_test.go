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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestJobCompletionTime(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			CompletionTime: &now,
		},
	}
	if got := jobCompletionTime(job); got == nil || !got.Equal(&now) {
		t.Fatalf("jobCompletionTime() = %v, want %v", got, now)
	}

	fallback := metav1.NewTime(now.Add(2 * time.Minute))
	job = &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: fallback,
				},
			},
		},
	}
	if got := jobCompletionTime(job); got == nil || !got.Equal(&fallback) {
		t.Fatalf("jobCompletionTime() from condition = %v, want %v", got, fallback)
	}
}

func TestJobCompletedAfter(t *testing.T) {
	t.Parallel()

	base := metav1.Now()
	earlier := metav1.NewTime(base.Add(-time.Minute))
	later := metav1.NewTime(base.Add(time.Minute))

	sync := &batchv1.Job{Status: batchv1.JobStatus{CompletionTime: &base}}
	source := &batchv1.Job{Status: batchv1.JobStatus{CompletionTime: &later}}

	if !jobCompletedAfter(sync, source) {
		t.Fatal("expected source completed after sync")
	}
	if jobCompletedAfter(sync, &batchv1.Job{Status: batchv1.JobStatus{CompletionTime: &earlier}}) {
		t.Fatal("expected source completed before sync")
	}
	if jobCompletedAfter(sync, &batchv1.Job{}) {
		t.Fatal("expected false when source has no completion time")
	}
}
