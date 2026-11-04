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

	corev1 "k8s.io/api/core/v1"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// Every identity Job runs the storage tools image, which carries curl and
// jq, as that image's user under the restricted context, and installs
// nothing when it runs. They ran a bare alpine:3.20, named by a tag anyone
// can move, as root, and fetched curl and jq from the network first.
func TestTheIdentityJobsRunTheToolsImageAsItsUser(t *testing.T) {
	scripts := map[string]string{
		"realm":          buildRealmScript("demo", "Demo"),
		"realm delete":   buildRealmDeleteScript("demo", "kernel"),
		"realm disable":  buildRealmDisableScript("demo"),
		"tenant admin":   buildAdminScript("demo"),
		"gentian groups": buildGentianGroupsScript("demo"),
	}
	for name, script := range scripts {
		c := keycloakContainer("job", script)
		if c.Image != kernel.StorageToolsImage() {
			t.Errorf("%s: runs %s, want %s", name, c.Image, kernel.StorageToolsImage())
		}
		command := strings.Join(c.Command, " ")
		if strings.Contains(command, "apk ") || strings.Contains(command, "apt-get ") {
			t.Errorf("%s: installs a package when it runs", name)
		}
		if !strings.HasSuffix(command, script) {
			t.Errorf("%s: does not run its script as it was built", name)
		}
		checkToolsUser(t, name, c.SecurityContext)
	}
}

func checkToolsUser(t *testing.T, name string, sc *corev1.SecurityContext) {
	t.Helper()
	switch {
	case sc == nil:
		t.Errorf("%s: no security context", name)
	case sc.RunAsUser == nil || *sc.RunAsUser != kernel.StorageToolsUser:
		t.Errorf("%s: does not run as %d", name, kernel.StorageToolsUser)
	case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot:
		t.Errorf("%s: runAsNonRoot is not true", name)
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Errorf("%s: allowPrivilegeEscalation is not false", name)
	case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		t.Errorf("%s: does not drop every capability", name)
	case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Errorf("%s: seccomp profile is not RuntimeDefault", name)
	}
}
