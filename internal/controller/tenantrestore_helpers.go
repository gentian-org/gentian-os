/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func setRestoreCondition(
	restore *gentianov1alpha1.TenantRestore,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i := range restore.Status.Conditions {
		c := &restore.Status.Conditions[i]
		if c.Type != condType {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status, c.Reason, c.Message = status, reason, message
		c.ObservedGeneration = restore.Generation
		return
	}
	restore.Status.Conditions = append(restore.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: restore.Generation,
	})
}

// quiesceModeFromMessage recovers how an app was actually paused.
//
// The mode is recorded in the status message when the pause happens, and read
// back here so the resume matches: an app paused by a maintenance command has
// to be taken out of maintenance, not merely scaled, or it comes back up still
// refusing writes.
func quiesceModeFromMessage(message string) gentianov1alpha1.BackupQuiesceMode {
	switch {
	case strings.Contains(message, string(gentianov1alpha1.BackupQuiesceCommand)):
		return gentianov1alpha1.BackupQuiesceCommand
	case strings.Contains(message, string(gentianov1alpha1.BackupQuiesceNone)):
		return gentianov1alpha1.BackupQuiesceNone
	default:
		return gentianov1alpha1.BackupQuiesceScaleDown
	}
}
