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
