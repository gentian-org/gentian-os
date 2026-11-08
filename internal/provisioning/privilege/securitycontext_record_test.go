/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package privilege

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What the privilege Job runs under, held to testdata/security-contexts.txt.
func TestSecurityContextTranscript(t *testing.T) {
	job := SyncJob("demo", "wiki", "tenant-demo", "f",
		&gentianov1alpha1.PrivilegedRoleSpec{Name: "admin"},
		&gentianov1alpha1.ProvisioningJobSpec{Image: "image", ServiceAccountName: "sa"})
	metatest.Hold(t, "testdata/security-contexts.txt", metatest.Objects(t, map[string]any{"sync": job}))
}
