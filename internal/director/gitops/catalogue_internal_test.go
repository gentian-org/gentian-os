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
