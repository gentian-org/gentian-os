/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TestVaultPathsTranscript holds the vault paths this package names to the
// strings they are.
func TestVaultPathsTranscript(t *testing.T) {
	source := func(name, tenant string) *gentianov1alpha1.BundleSource {
		s := &gentianov1alpha1.BundleSource{ObjectMeta: metav1.ObjectMeta{Name: name}}
		s.Spec.Tenant = tenant
		return s
	}
	for _, c := range []struct{ got, want string }{
		{SourceVaultPath(source("archive", "")), "gentian-os/kernel/backup/sources/archive"},
		{SourceVaultPath(source("demo.archive", "demo")), "gentian-os/tenants/demo/backup/sources/archive"},
		{SourceVaultPath(source("archive", "demo")), "gentian-os/tenants/demo/backup/sources/archive"},
		{SourceVaultPath(source("demo.demo.archive", "demo")), "gentian-os/tenants/demo/backup/sources/demo.archive"},
		{DestinationVaultPath("tenant", "demo"), "gentian-os/tenants/demo/backup/destination"},
		{DestinationVaultPath("tenant", ""), "gentian-os/kernel/backup/destination"},
		{DestinationVaultPath("cluster", "demo"), "gentian-os/kernel/backup/destination"},
		{DestinationVaultPath("", ""), "gentian-os/kernel/backup/destination"},
		{SigningKeyPath, "gentian-os/kernel/backup/signing"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
