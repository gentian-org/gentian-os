/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package provisioner

import (
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What a cleanup's wait reads of a Job's state.
func TestJobStatusTranscript(t *testing.T) {
	got := metatest.Transcript(map[string]func(*batchv1.Job) string{
		"failed": func(j *batchv1.Job) string { return fmt.Sprint(jobFailed(j)) },
		"reason": func(j *batchv1.Job) string { return fmt.Sprintf("%q", jobFailureReason(j)) },
	})
	want := `complete, false: failed=false reason=""
complete: failed=false reason=""
failed after another: failed=true reason="R   m"
failed, no message: failed=true reason="DeadlineExceeded"
failed, unknown: failed=false reason=""
failed: failed=true reason="BackoffLimitExceeded Job has reached the specified backoff limit"
new: failed=false reason=""
running, one failure: failed=false reason=""
succeeded, no condition yet: failed=false reason=""
suspended: failed=false reason=""
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
