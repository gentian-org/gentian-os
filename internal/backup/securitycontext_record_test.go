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

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta/metatest"
)

// What every pod the backup Jobs run under, held to
// testdata/security-contexts.txt.
func TestSecurityContextTranscript(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	objs := map[string]any{
		"tools":                    toolsContainer("c", "true"),
		"storage tools":            storageTools("c", "true", []corev1.EnvVar{{Name: "A", Value: "b"}}),
		"volume tools":             volumeToolsContainer("c", "true"),
		"mail":                     mailContainer("c", "true"),
		"object storage provision": ObjectStorageProvisionContainerWith("c", "bucket", "keys", ObjectStorageOptions{}),
		"object storage destroy":   ObjectStorageDestroyJob(tenant, "wiki", 60),
		"cache destroy":            CacheDestroyJob(tenant, "wiki", 60),
		"mailbox destroy":          MailboxDestroyJob("ns", "demo", "claim", "node", "example.org", 60),
		"hardened, empty":          hardened([]corev1.Container{{Name: "a"}}),
		"hardened, already set":    hardened([]corev1.Container{{Name: "a", SecurityContext: &corev1.SecurityContext{Privileged: new(bool)}}}),
	}
	metatest.Hold(t, "testdata/security-contexts.txt", metatest.Objects(t, objs))
}
