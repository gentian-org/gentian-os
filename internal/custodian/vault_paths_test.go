/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import "testing"

// TestVaultPathsTranscript holds the vault paths this package names to the
// strings they are.
func TestVaultPathsTranscript(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{BackupIdentityPath("demo"), "gentian-os/tenants/demo/backup/identity"},
		{BackupIdentityPath(""), "gentian-os/tenants//backup/identity"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
