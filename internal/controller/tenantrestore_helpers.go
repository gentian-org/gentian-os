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
