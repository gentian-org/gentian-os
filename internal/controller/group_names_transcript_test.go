/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TestGroupNamesTranscript holds the group paths a sign-in gives.
func TestGroupNamesTranscript(t *testing.T) {
	target := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	target.Spec.Groups = []gentianov1alpha1.TenantGroup{{Name: "staff"}}
	got := fmt.Sprint(signInGroupPaths(target, []string{"members", "staff", "other", "members"}))
	if want := "[/gentian:tenant:acme:members /staff]"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
