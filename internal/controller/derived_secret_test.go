/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import "testing"

// The value below was computed from the implementation that hardcoded
// `if appName == "open-webui"`, before the key became declarable. It is pinned
// here because this is a session-signing key: changing the derivation rotates it
// for every existing tenant and logs all their users out. A failure of this test
// is not a stale expectation to update — it means the change under test would
// break live sessions.
func TestDerivedSecretValueIsFrozen(t *testing.T) {
	t.Parallel()
	const want = "LdpVVeWPrRuSGJJbJhDDF_Pvr7DmSyEMHrFmkyfAZ18="
	if got := derivedSecretValue("demo", "open-webui"); got != want {
		t.Fatalf("derivation changed: got %q, want %q — this rotates every tenant's key", got, want)
	}
}

func TestDerivedSecretValueVariesByTenantAndApp(t *testing.T) {
	t.Parallel()
	a := derivedSecretValue("demo", "open-webui")
	if b := derivedSecretValue("other", "open-webui"); a == b {
		t.Fatal("two tenants must not share a derived secret")
	}
	if b := derivedSecretValue("demo", "other-app"); a == b {
		t.Fatal("two apps in one tenant must not share a derived secret")
	}
}
