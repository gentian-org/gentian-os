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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

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
