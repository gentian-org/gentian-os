/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package metatest holds the Job statuses every reader of a Job's state is held
// to in its transcript.
package metatest

import (
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// JobStatuses are the Job statuses every reader of a Job's state is held
// to, by name.
func JobStatuses() map[string]*batchv1.Job {
	cond := func(t batchv1.JobConditionType, s corev1.ConditionStatus, reason, message string) batchv1.JobCondition {
		return batchv1.JobCondition{Type: t, Status: s, Reason: reason, Message: message}
	}
	job := func(succeeded, failed int32, conds ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Succeeded: succeeded, Failed: failed, Conditions: conds}}
	}
	return map[string]*batchv1.Job{
		"new":                         job(0, 0),
		"running, one failure":        job(0, 1),
		"complete":                    job(1, 0, cond(batchv1.JobComplete, corev1.ConditionTrue, "", "")),
		"succeeded, no condition yet": job(1, 0),
		"complete, false":             job(0, 0, cond(batchv1.JobComplete, corev1.ConditionFalse, "", "")),
		"failed":                      job(0, 3, cond(batchv1.JobFailed, corev1.ConditionTrue, "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
		"failed, no message":          job(0, 3, cond(batchv1.JobFailed, corev1.ConditionTrue, "DeadlineExceeded", "")),
		"failed, unknown":             job(0, 3, cond(batchv1.JobFailed, corev1.ConditionUnknown, "R", "m")),
		"suspended":                   job(0, 0, cond(batchv1.JobSuspended, corev1.ConditionTrue, "", "")),
		"failed after another":        job(0, 3, cond(batchv1.JobSuspended, corev1.ConditionTrue, "", ""), cond(batchv1.JobFailed, corev1.ConditionTrue, " R ", " m ")),
	}
}

// Transcript is what each reader says of each status, one line a status,
// sorted by name.
func Transcript(readers map[string]func(*batchv1.Job) string) string {
	var names []string
	for name := range readers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for status, job := range JobStatuses() {
		parts := []string{status + ":"}
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s=%s", name, readers[name](job)))
		}
		out = append(out, strings.Join(parts, " "))
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}
