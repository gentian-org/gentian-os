/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"reflect"
	"testing"
)

func TestAFieldIsTakenOutWithItsValueAndItsComment(t *testing.T) {
	text := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  # What the tenant may use.
  quotas:
    cpu: "16"
    # No longer known.
    # In two lines.
    gone: 20
    storage: 50Gi
  note: |
    first
    second
  nested:
    a: 1
    b:
    - x
    - y
  # Apps are installed through the director.
  apps:
  - profile: one
    extra: {k: v}
    bogus: true
  - bogus: true
    profile: two
  braces: {gone: 1, kept: 2}
`
	want := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  # What the tenant may use.
  quotas:
    cpu: "16"
    storage: 50Gi
  # Apps are installed through the director.
  apps:
  - profile: one
    extra: {k: v}
  - bogus: true
    profile: two
  braces: {gone: 1, kept: 2}
`
	next, taken, left := takeOutFields(text, []string{
		"spec.quotas.gone", "spec.note", "spec.nested", "spec.apps[0].bogus",
		"spec.apps[1].bogus", "spec.braces.gone", "spec.absent",
	})
	if next != want {
		t.Errorf("the text:\n%s\nwant:\n%s", next, want)
	}
	if want := []string{"spec.apps[0].bogus", "spec.nested", "spec.note", "spec.quotas.gone"}; !reflect.DeepEqual(taken, want) {
		t.Errorf("taken %v, want %v", taken, want)
	}
	// The first field of a list entry, a field in braces and one that is not
	// there are left, and named.
	if want := []string{"spec.apps[1].bogus", "spec.braces.gone", "spec.absent"}; !reflect.DeepEqual(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

func TestATextWithNothingToTakeOutIsLeftAsItIs(t *testing.T) {
	text := "spec:\n  quotas:\n    cpu: \"16\"\n"
	next, taken, left := takeOutFields(text, nil)
	if next != text || taken != nil || left != nil {
		t.Errorf("%q %v %v", next, taken, left)
	}
}
