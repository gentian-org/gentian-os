/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import "testing"

// TestVaultPathsTranscript holds the vault paths this package names to the
// strings they are.
func TestVaultPathsTranscript(t *testing.T) {
	if BackupRecipientsPath != "gentian-os/kernel/backup/recipients" {
		t.Errorf("got %q", BackupRecipientsPath)
	}
}
