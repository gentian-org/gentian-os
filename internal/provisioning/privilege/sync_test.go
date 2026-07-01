// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package privilege

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/authz"
)

func TestNextcloudUID_PrefersGentianUsername(t *testing.T) {
	t.Parallel()
	uid := NextcloudUID(authz.KeycloakUser{
		Username:   "john-doe@demo.example",
		Email:      "john-doe@demo.example",
		Attributes: map[string][]string{"gentian_username": {"john-doe"}},
	})
	if uid != "john-doe" {
		t.Fatalf("NextcloudUID() = %q", uid)
	}
}

func TestMemberFingerprint_StableOrdering(t *testing.T) {
	t.Parallel()
	a := MemberFingerprint([]authz.KeycloakUser{{ID: "b"}, {ID: "a"}})
	b := MemberFingerprint([]authz.KeycloakUser{{ID: "a"}, {ID: "b"}})
	if a != b {
		t.Fatalf("fingerprints differ: %q vs %q", a, b)
	}
}
