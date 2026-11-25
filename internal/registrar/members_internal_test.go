/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"strings"
	"testing"
)

// The platform tenant's people are the cluster's own, who enter by the
// roles the Cluster claim gives them: an invitation there gives what it
// names and nothing unasked. Everywhere else it gives the position user.
func TestThePlatformTenantsPeopleAreGivenNothingUnasked(t *testing.T) {
	if got, ok := invitedPositions(platformTenant, nil); !ok || len(got) != 0 {
		t.Fatalf("positions = %v %v", got, ok)
	}
	if got, ok := invitedPositions(platformTenant, []string{"operators"}); !ok || strings.Join(got, ",") != "operators" {
		t.Fatalf("positions = %v %v", got, ok)
	}
	if got, ok := invitedPositions("demo", nil); !ok || strings.Join(got, ",") != "user" {
		t.Fatalf("positions = %v %v", got, ok)
	}
	if _, ok := invitedPositions("demo", []string{"a/b"}); ok {
		t.Fatal("a name that is none is taken for a position")
	}
}
