/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"errors"
	"testing"
)

func TestIdentifiersCrossIntoOpenFGAThroughOneMapping(t *testing.T) {
	for in, want := range map[string]string{
		"gentian:tenant:demo:admins":               "group:gentian/tenant/demo/admins",
		"gentian:tenant:demo:app:nextcloud:admins": "group:gentian/tenant/demo/app/nextcloud/admins",
		"gentian:platform:break-glass":             "group:gentian/platform/break-glass",
	} {
		if got, err := Group(in); err != nil || got != want {
			t.Errorf("Group(%q) = %q, %v", in, got, err)
		}
		if got, err := GroupFromPath("/" + in); err != nil || got != want {
			t.Errorf("GroupFromPath(/%q) = %q, %v", in, got, err)
		}
	}
	// A federated Keycloak user id carries colons too.
	if got, _ := User("f:ldap-1:jdoe"); got != "user:f/ldap-1/jdoe" {
		t.Errorf("User = %q", got)
	}
}

// Two different names must never become the same id, and nothing may smuggle a
// relation or a second object into a tuple.
func TestWhatCannotBeRepresentedIsRefused(t *testing.T) {
	for _, bad := range []string{"", "a/b", "gentian:tenant:demo#member", "a b", "a\nb", "parent/child"} {
		if _, err := Group(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Group(%q): %v", bad, err)
		}
		if _, err := User(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("User(%q): %v", bad, err)
		}
	}
}
