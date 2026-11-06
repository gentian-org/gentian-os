/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package privilege

import (
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What a privilege Job's state is read as.
func TestJobStatusTranscript(t *testing.T) {
	got := metatest.Transcript(map[string]func(*batchv1.Job) string{
		"state":   func(j *batchv1.Job) string { return fmt.Sprint(StateOf(j)) },
		"message": func(j *batchv1.Job) string { return fmt.Sprintf("%q", FailureMessage(j)) },
	})
	want := `complete, false: message="0 attempt(s) failed" state=0
complete: message="0 attempt(s) failed" state=1
failed after another: message=" m " state=2
failed, no message: message="3 attempt(s) failed" state=2
failed, unknown: message="3 attempt(s) failed" state=0
failed: message="Job has reached the specified backoff limit" state=2
new: message="0 attempt(s) failed" state=0
running, one failure: message="1 attempt(s) failed" state=0
succeeded, no condition yet: message="0 attempt(s) failed" state=1
suspended: message="0 attempt(s) failed" state=0
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
