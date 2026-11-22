/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"testing"
)

type recordingChecker struct{ asked []string }

func (c *recordingChecker) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	c.asked = append(c.asked, user+" "+relation+" "+object)
	return false, nil
}

// TestTenantAdministratorQuestion holds what the store is asked to learn
// whether a person administers a tenant: the role, on the tenant.
func TestTenantAdministratorQuestion(t *testing.T) {
	c := &recordingChecker{}
	if _, err := TenantAdministrator(context.Background(), c, "", "acme", "s-1"); err != nil {
		t.Fatal(err)
	}
	if len(c.asked) != 1 || c.asked[0] != "user:s-1 admin tenant:acme" {
		t.Fatalf("asked %q", c.asked)
	}
	// A tenant that is no name is asked about nothing.
	if ok, err := TenantAdministrator(context.Background(), c, "", "ac:me", "s-1"); ok || err != nil || len(c.asked) != 1 {
		t.Fatalf("a tenant that cannot be named: %v %v %q", ok, err, c.asked)
	}
}
