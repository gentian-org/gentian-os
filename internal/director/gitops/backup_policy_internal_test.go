/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"errors"
	"strings"
	"testing"
)

// What is exported before a move is committed as stated, and only one of
// the three modes is.
func TestABackupPolicyCommitsWhatIsExportedBeforeAMove(t *testing.T) {
	body := renderBackupPolicy("tenant", "demo", BackupPolicy{BeforeMove: "none"})
	if !strings.Contains(body, "\n  beforeMove: none\n") {
		t.Fatalf("rendered:\n%s", body)
	}
	if body := renderBackupPolicy("tenant", "demo", BackupPolicy{}); strings.Contains(body, "beforeMove") {
		t.Fatalf("an inherited mode was written:\n%s", body)
	}
	for _, mode := range []string{"", "tenant", "app", "none"} {
		if err := (BackupPolicy{BeforeMove: mode}).validate(); err != nil {
			t.Errorf("%q refused: %v", mode, err)
		}
	}
	if err := (BackupPolicy{BeforeMove: "everything"}).validate(); !errors.Is(err, ErrInvalidBackupPolicy) {
		t.Fatalf("an unknown mode: %v", err)
	}
}
