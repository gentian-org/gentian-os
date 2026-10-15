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

// What makes a restore other than the guaranteed round trip is said in a
// field of its own, and does not take the place of where the restore is.
func TestARestoreSaysWhereItIsOutsideTheGuaranteeApartFromWhereItIs(t *testing.T) {
	r := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "import-1"}}
	r.Status.Conditions = []metav1.Condition{
		{Type: "OutsideGuarantee", Status: metav1.ConditionTrue, Reason: "OtherVersion", Message: "written by platform version 0.4"},
		{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", Message: "restoring 2 app(s)"},
	}
	got := restoreStatus(r, "demo")
	if got.OutsideGuarantee != "written by platform version 0.4" || got.Message != "restoring 2 app(s)" {
		t.Errorf("outsideGuarantee %q, message %q", got.OutsideGuarantee, got.Message)
	}

	r.Status.Conditions = r.Status.Conditions[1:]
	if got := restoreStatus(r, "demo"); got.OutsideGuarantee != "" {
		t.Errorf("a restore within the guarantee says %q", got.OutsideGuarantee)
	}
}
