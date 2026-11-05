/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// The longest tenant name is the longest whose two tightest derived names
// still fit: the desktop's Helm release (Helm: 53) and the backup bucket's
// purge Job (the job-name pod label: 63). One character more and both
// overflow, which is why MaxTenantNameLength is what it is.
func TestTheLongestTenantNameFitsTheNamesBuiltFromIt(t *testing.T) {
	const helmRelease, jobName = 53, 63
	derived := func(tenant string) (release, purge string) {
		ns := (&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: tenant}}).NamespaceName()
		desktop := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: DesktopComponentName, Namespace: ns}}
		return releaseName(desktop), backup.ObjectStorageDestroyJobName(tenant, backupBucketUnit)
	}

	longest := strings.Repeat("a", gentianov1alpha1.MaxTenantNameLength)
	if !gentianov1alpha1.ValidTenantName(longest) {
		t.Fatalf("a name of MaxTenantNameLength is not valid")
	}
	release, purge := derived(longest)
	if len(release) > helmRelease || len(purge) > jobName {
		t.Fatalf("at %d: release %q is %d (limit %d), purge Job %q is %d (limit %d)",
			len(longest), release, len(release), helmRelease, purge, len(purge), jobName)
	}

	one := longest + "a"
	if gentianov1alpha1.ValidTenantName(one) {
		t.Fatalf("a name one longer than MaxTenantNameLength is valid")
	}
	release, purge = derived(one)
	if len(release) <= helmRelease || len(purge) <= jobName {
		t.Fatalf("at %d both names should overflow, and the limit could be higher: release %d, purge Job %d",
			len(one), len(release), len(purge))
	}
}
