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

// The script matches on the username, so a renamed member must run it again.
func TestMemberFingerprint_ChangesWithUsername(t *testing.T) {
	t.Parallel()
	a := MemberFingerprint([]authz.KeycloakUser{{ID: "a", Username: "alice@corp.example"}})
	b := MemberFingerprint([]authz.KeycloakUser{{ID: "a", Username: "alice@other.example"}})
	if a == b {
		t.Fatalf("fingerprint ignores the username: %q", a)
	}
}

// A profile whose sync script changes has not had the change applied, so the
// sync must run again although the members are the same.
func TestDeclarationFingerprint_ChangesWithTheScript(t *testing.T) {
	t.Parallel()
	type job struct{ Script string }
	a := DeclarationFingerprint(job{Script: "give System Manager"})
	b := DeclarationFingerprint(job{Script: "give every role"})
	if a == "" || a == b {
		t.Fatalf("fingerprint ignores the script: %q vs %q", a, b)
	}
	if a != DeclarationFingerprint(job{Script: "give System Manager"}) {
		t.Fatal("fingerprint is not stable")
	}
}
