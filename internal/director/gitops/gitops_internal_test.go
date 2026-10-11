/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"strings"
	"testing"
)

// Every git the director runs is told to do its housekeeping before it
// returns. Without that a repack is left running in the checkout after the
// command that started it has answered, which nothing waits for.
func TestGitDoesItsHousekeepingBeforeItReturns(t *testing.T) {
	g := NewGitOps(t.TempDir(), "", "", Person{})
	for _, key := range []string{"maintenance.autoDetach", "gc.autoDetach"} {
		cmd, cancel := g.gitCmd(context.Background(), "config", "--get", key)
		out, err := cmd.Output()
		cancel()
		if err != nil || strings.TrimSpace(string(out)) != "false" {
			t.Errorf("%s = %q (%v), want false", key, strings.TrimSpace(string(out)), err)
		}
	}
}
