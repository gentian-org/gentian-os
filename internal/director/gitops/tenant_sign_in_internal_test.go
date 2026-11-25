/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What an entry asks the source's people to hold is written with it and
// read back: a patch rewritten without it would give the positions to
// everybody from the source.
func TestASignInsHoldingIsWrittenAndReadBack(t *testing.T) {
	t.Parallel()
	var doc signInPatch
	doc.Spec.SignInFrom = []gentianov1alpha1.TenantSignInFrom{
		{Tenant: "team", Positions: []string{"website-editor"}, Holding: []string{"sales", "executive"}},
	}
	out := renderSignIn("customers", doc)
	if !strings.Contains(out, "      holding: [sales, executive]\n") {
		t.Fatalf("holding is not written:\n%s", out)
	}
	var back signInPatch
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Spec.SignInFrom) != 1 || !slices.Equal(back.Spec.SignInFrom[0].Holding, []string{"sales", "executive"}) {
		t.Fatalf("read back %+v", back.Spec.SignInFrom)
	}
}
