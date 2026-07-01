// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
