/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
	if got, _ := CatalogueEntry("main/nextcloud"); got != "catalogue_entry:main/nextcloud" {
		t.Errorf("CatalogueEntry = %q", got)
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
	for _, bad := range []string{"", "nextcloud", "main/", "/x", "main:nextcloud", "a/b/c", "main/x#can_install"} {
		if _, err := CatalogueEntry(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("CatalogueEntry(%q): %v", bad, err)
		}
	}
}
