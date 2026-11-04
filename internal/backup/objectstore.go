/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// How the Jobs that move a tenant's objects reach an object store.
//
// They run the platform's storage tools (kernel.StorageToolsImage: rclone,
// age, tar) and speak plain S3, whichever server is behind the endpoint: the
// cluster's own, or a destination a backup policy names. Nothing is fetched
// when a Job runs.
//
// A container is handed S3_ENDPOINT, S3_ACCESS_KEY and S3_SECRET_KEY, from
// Secret references (and S3_REGION and S3_ADDRESSING_STYLE where a
// destination names them), and its
// script begins with rcloneRemote, which turns them into an rclone remote
// through rclone's environment. No key is ever on a command line.

// Remotes, by what they address.
const (
	// bundleRemote is wherever the bundle lives (bundleEnv).
	bundleRemote = "bundle"
	// platformRemote is the cluster's own object store, where the apps'
	// buckets are (PlatformStorageEnv).
	platformRemote = "platform"
)

// rcloneRemote is the script lines that define remote from the S3_*
// variables of the container's environment.
//
// RCLONE_S3_NO_CHECK_BUCKET: without it rclone asks to create a bucket
// before it writes into one. A key scoped to objects -- an app's own, or the
// credential of a destination somebody else owns -- is refused that, and the
// write fails before it starts. The one place a bucket is made on purpose
// (bucketPreparation) says so for that command.
//
// Path-style addressing, stated rather than left to rclone's default for a
// provider it does not name, unless S3_ADDRESSING_STYLE says virtualHost (a
// destination that serves a bucket only by its own host name); and the
// region when one is given.
func rcloneRemote(remote string) string { return rcloneRemoteFrom(remote, "S3") }

// rcloneRemoteFrom is rcloneRemote from the variables named <vars>_ENDPOINT,
// <vars>_ACCESS_KEY and so on: for a container that addresses two stores.
func rcloneRemoteFrom(remote, vars string) string {
	r := "RCLONE_CONFIG_" + strings.ToUpper(remote)
	return fmt.Sprintf(`export %[1]s_TYPE=s3 %[1]s_PROVIDER=Other RCLONE_S3_NO_CHECK_BUCKET=true
export %[1]s_ENDPOINT="${%[2]s_ENDPOINT}" %[1]s_ACCESS_KEY_ID="${%[2]s_ACCESS_KEY}" %[1]s_SECRET_ACCESS_KEY="${%[2]s_SECRET_KEY}"
if [ -n "${%[2]s_REGION:-}" ]; then export %[1]s_REGION="${%[2]s_REGION}"; fi
if [ "${%[2]s_ADDRESSING_STYLE:-path}" = virtualHost ]; then export %[1]s_FORCE_PATH_STYLE=false; else export %[1]s_FORCE_PATH_STYLE=true; fi`, r, vars)
}

// storageEnv addresses an object store whose address and keys are all in
// one Secret: the platform's own.
func storageEnv(secret string) []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("S3_ENDPOINT", secret, "endpoint"),
		meta.SecretEnv("S3_ACCESS_KEY", secret, "accessKey"),
		meta.SecretEnv("S3_SECRET_KEY", secret, "secretKey"),
	}
}

// storageTools is a container of the platform's storage tools running
// script with env.
func storageTools(name, script string, env []corev1.EnvVar) corev1.Container {
	c := toolsContainer(name, script)
	c.Env = env
	return c
}

// toolsContainer is a container of the platform's storage tools running
// script in the scratch directory, as the image's own unprivileged user
// (toolsSecurityContext). The image carries every tool the scripts call;
// nothing is installed when one runs.
func toolsContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           kernel.StorageToolsImage(),
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{script},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
		SecurityContext: toolsSecurityContext(),
	}
}

// toolsSecurityContext is the restricted context a storage tools container
// runs under: the image's user and group, no privilege escalation, no
// capabilities, the runtime's seccomp profile. What it writes goes to the
// scratch directory (an emptyDir, writable by anyone) and /tmp (the image's,
// writable by anyone); it reads what an earlier step of the same pod wrote
// there.
func toolsSecurityContext() *corev1.SecurityContext {
	no, yes := false, true
	user := kernel.StorageToolsUser
	return &corev1.SecurityContext{
		RunAsUser:                &user,
		RunAsGroup:               &user,
		RunAsNonRoot:             &yes,
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// volumeToolsContainer is a storage tools container that reads or writes an
// app's volume, which only root can do: the files are whichever uid the app
// writes as, some readable by that uid alone, and a restore puts each back
// under the owner it had. It runs as root without privilege escalation, and
// keeps the runtime's default capabilities, which are what lets root read and
// chown files it does not own. The volume Jobs run in tenant namespaces,
// whose policies require neither non-root nor dropped capabilities.
func volumeToolsContainer(name, script string) corev1.Container {
	c := toolsContainer(name, script)
	no := false
	root := int64(0)
	c.SecurityContext = &corev1.SecurityContext{
		RunAsUser:                &root,
		RunAsGroup:               &root,
		AllowPrivilegeEscalation: &no,
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	return c
}
