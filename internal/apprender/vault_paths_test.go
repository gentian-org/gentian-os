/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import "testing"

// TestVaultPathsTranscript holds the vault paths this package names to the
// strings they are. The rendered goldens hold the paths the renders spell
// in place.
func TestVaultPathsTranscript(t *testing.T) {
	r := &valuesRender{tenant: "demo", in: &Input{App: "demo-app"}}
	for _, c := range []struct{ got, want string }{
		{r.appPath("smtp"), "gentian-os/tenants/demo/apps/demo-app/smtp"},
		{r.appPath("oidc"), "gentian-os/tenants/demo/apps/demo-app/oidc"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
