/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package envvar

import "testing"

func TestAnEmptyVariableIsAnUnsetOne(t *testing.T) {
	t.Setenv("ENVVAR_TEST", "")
	if got := Or("ENVVAR_TEST", "default"); got != "default" {
		t.Fatalf("empty: %q", got)
	}
	if got := Or("ENVVAR_TEST_NEVER_SET", "default"); got != "default" {
		t.Fatalf("unset: %q", got)
	}
	t.Setenv("ENVVAR_TEST", "set")
	if got := Or("ENVVAR_TEST", "default"); got != "set" {
		t.Fatalf("set: %q", got)
	}
}
