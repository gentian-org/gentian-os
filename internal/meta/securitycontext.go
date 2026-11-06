/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package meta

import corev1 "k8s.io/api/core/v1"

// RestrictedContainer is what a Job's container runs under in a namespace
// that enforces the restricted Pod Security Standard: no privilege
// escalation, no capabilities, the runtime's seccomp profile. A pod refused
// there never exists, and its Job sits Running with nothing behind it.
func RestrictedContainer() *corev1.SecurityContext {
	no := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// RestrictedContainerAs is RestrictedContainer as the image's unprivileged
// user and group uid.
func RestrictedContainerAs(uid int64) *corev1.SecurityContext {
	sc := RestrictedContainer()
	yes := true
	sc.RunAsUser, sc.RunAsGroup, sc.RunAsNonRoot = &uid, &uid, &yes
	return sc
}

// ReadOnlyContainer is what a long-running service's container runs under:
// no privilege escalation, no capabilities, a read-only root file system.
// Its pod's NonRootPod sets the user and the seccomp profile.
func ReadOnlyContainer() *corev1.SecurityContext {
	no, yes := false, true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &yes,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// NonRootPod is the pod security context of a long-running service: the
// image's unprivileged user uid, never root, the runtime's seccomp profile.
func NonRootPod(uid int64) *corev1.PodSecurityContext {
	yes := true
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   &yes,
		RunAsUser:      &uid,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
