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

// TestGroupNamesTranscript holds the groups the store is asked about: an
// app's, and a tenant's administrators'.
func TestGroupNamesTranscript(t *testing.T) {
	if got, err := appGroup("acme", "wiki"); err != nil || got != "group:gentian/tenant/acme/app/wiki" {
		t.Fatalf("appGroup: %q %v", got, err)
	}
	c := &recordingChecker{}
	if _, err := TenantAdministrator(context.Background(), c, "", "acme", "s-1"); err != nil {
		t.Fatal(err)
	}
	if len(c.asked) != 1 || c.asked[0] != "user:s-1 member group:gentian/tenant/acme/admins" {
		t.Fatalf("asked %q", c.asked)
	}
}
