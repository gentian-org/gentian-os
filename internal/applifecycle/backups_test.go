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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A screen that offers a person their own exports must tell them apart from
// the ones a schedule took.
func TestABackupSaysWhichScheduleTookIt(t *testing.T) {
	scheduled := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{
		Name:   "nightly-20261105-0300",
		Labels: map[string]string{gentianov1alpha1.TenantExportScheduleLabel: "nightly"},
	}}
	if got := backupSummary(scheduled, "demo").Schedule; got != "nightly" {
		t.Errorf("scheduled export: Schedule = %q, want %q", got, "nightly")
	}

	manual := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "export-2026-11-05-10-00"}}
	if got := backupSummary(manual, "demo").Schedule; got != "" {
		t.Errorf("manual export: Schedule = %q, want empty", got)
	}
}
