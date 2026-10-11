/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package privilege

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/authz"
)

func TestMemberFingerprint_StableOrdering(t *testing.T) {
	t.Parallel()
	a := MemberFingerprint([]authz.KeycloakUser{{ID: "b"}, {ID: "a"}})
	b := MemberFingerprint([]authz.KeycloakUser{{ID: "a"}, {ID: "b"}})
	if a != b {
		t.Fatalf("fingerprints differ: %q vs %q", a, b)
	}
}
