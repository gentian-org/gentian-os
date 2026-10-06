/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"strings"
	"testing"
)

// Step 0 scaffolds the catalogue's kustomization with an empty list so the
// directory exists before anything is materialised; the first entry has to
// turn that into a block list, not add a second resources key.
func TestTheFirstEntryFillsAnEmptyResourceList(t *testing.T) {
	out, changed := ensureResourceListed("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n", "operations-console.yaml")
	if !changed {
		t.Fatal("nothing was listed")
	}
	if strings.Count(out, "resources:") != 1 || !strings.Contains(out, "resources:\n- operations-console.yaml\n") {
		t.Fatalf("kustomization:\n%s", out)
	}
}
