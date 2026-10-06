/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/gentian-org/gentian-os/api/bundle"
)

// The manifest's format is part of the API, so that anything reading or
// writing a bundle agrees on it. The names here are the ones this package has
// always used.
type (
	Manifest         = bundle.Manifest
	ManifestApp      = bundle.ManifestApp
	ManifestStore    = bundle.ManifestStore
	ManifestIdentity = bundle.ManifestIdentity
)

// ManifestSchemaVersion is the version this build writes and reads.
const ManifestSchemaVersion = bundle.SchemaVersion

// ManifestJob writes the manifest into the bundle.
//
// It runs last. A bundle without a manifest is one a restore will refuse, so
// its presence is what marks the bundle complete — there is no separate flag to
// disagree with.
// The manifest is encrypted like every other artefact — it carries the tenant's
// spec and its app inventory, which is not something to leave readable next to
// an encrypted bundle. info is written in the clear beside it, and says only
// what the bundle is and how to open it.
func ManifestJob(p JobParams, m *Manifest, info *BundleInfo) (*batchv1.Job, error) {
	// Compact, so the heredocs below can never contain a line matching their
	// own delimiter no matter what a display name holds.
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	encodedInfo, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("encode bundle info: %w", err)
	}

	stage := corev1.Container{
		Name:    "stage-manifest",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
cat <<'GENTIAN_MANIFEST_EOF' > %s/manifest.json
%s
GENTIAN_MANIFEST_EOF
echo "staged manifest"`, workDir, string(encoded))},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}

	job := uploadJob(p, "manifest.json", "manifest.json", []corev1.Container{stage}, nil)

	// bundle-info.json goes up unencrypted, in the same Job, after the manifest.
	// Someone holding only this prefix can then tell whose bundle it is and
	// which key opens it, without being able to read a byte of the contents.
	job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, corev1.Container{
		Name:    "bundle-info",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
cat <<'GENTIAN_INFO_EOF' | mc pipe "gentian/%s/%s/bundle-info.json"
%s
GENTIAN_INFO_EOF
echo "wrote bundle info"`, p.Bucket, p.Prefix, string(encodedInfo))},
		Env: bundleEnv(p),
	})
	return job, nil
}
