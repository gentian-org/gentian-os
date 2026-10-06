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

// The bundle is a kustomize patch, and kustomize finds a patch's target by
// everything the patch states about it. A profile that states a namespace --
// the kind has none, and a document may still say one -- is only found by a
// patch that states the same; otherwise the whole catalogue directory stops
// building, for every entry in it.
func TestTheBundleNamesItsProfileAsTheProfileNamesItself(t *testing.T) {
	plain, err := renderBundle("element", []byte("apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: element\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: element\n  annotations:\n") {
		t.Fatalf("bundle:\n%s", plain)
	}
	odd, err := renderBundle("element", []byte("apiVersion: gentianos.io/v1beta7\nkind: ComponentProfile\nmetadata:\n  name: element\n  namespace: odd\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(odd), "apiVersion: gentianos.io/v1beta7\n") || !strings.Contains(string(odd), "  name: element\n  namespace: odd\n") {
		t.Fatalf("bundle:\n%s", odd)
	}
	if _, err := renderBundle("element", []byte("kind: ComponentProfile\n")); err == nil {
		t.Fatal("a bundle was rendered for a document that states no apiVersion")
	}
}
