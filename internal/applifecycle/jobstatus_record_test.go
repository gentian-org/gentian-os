/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What a fetch's status says of its Job.
func TestFetchStatusTranscript(t *testing.T) {
	got := metatest.Transcript(map[string]func(*batchv1.Job) string{
		"phase": func(j *batchv1.Job) string { st := fetchStatus(j); return st.Phase + " / " + st.Message },
	})
	want := `complete, false: phase=running / copying the bundle into the cluster's storage
complete: phase=ready / the bundle is in the cluster's storage
failed after another: phase=failed / the bundle could not be copied: R   m. The fetch's log says what the source answered
failed, no message: phase=failed / the bundle could not be copied: DeadlineExceeded. The fetch's log says what the source answered
failed, unknown: phase=running / copying the bundle into the cluster's storage
failed: phase=failed / the bundle could not be copied: BackoffLimitExceeded Job has reached the specified backoff limit. The fetch's log says what the source answered
new: phase=running / copying the bundle into the cluster's storage
running, one failure: phase=running / copying the bundle into the cluster's storage
succeeded, no condition yet: phase=running / copying the bundle into the cluster's storage
suspended: phase=running / copying the bundle into the cluster's storage
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
