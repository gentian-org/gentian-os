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
//
// With a key, the manifest is signed (bundle.SignatureFile): the signature is
// written beside bundle-info.json, in the clear, by the same container. It
// is over the bytes the manifest is staged with, so it is made here and
// nowhere else. Without one the bundle is unsigned.
func ManifestJob(p JobParams, m *Manifest, info *BundleInfo, key *SigningKey) (*batchv1.Job, error) {
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

	stage := storageTools("stage-manifest", fmt.Sprintf(`set -eu
cat <<'GENTIAN_MANIFEST_EOF' > %s/manifest.json
%s
GENTIAN_MANIFEST_EOF
echo "staged manifest"`, workDir, string(encoded)), nil)

	job := uploadJob(p, "manifest.json", "manifest.json", []corev1.Container{stage}, nil)

	// bundle-info.json goes up unencrypted, in the same Job, after the manifest.
	// Someone holding only this prefix can then tell whose bundle it is and
	// which key opens it, without being able to read a byte of the contents.
	signature := ""
	if key != nil {
		encodedSig, err := json.Marshal(key.SignManifest(encoded))
		if err != nil {
			return nil, fmt.Errorf("encode the manifest's signature: %w", err)
		}
		signature = fmt.Sprintf(`
cat <<'GENTIAN_SIGNATURE_EOF' | rclone rcat "%s:%s/%s/%s"
%s
GENTIAN_SIGNATURE_EOF
echo "wrote the manifest's signature"`, bundleRemote, p.Bucket, p.Prefix, bundle.SignatureFile, string(encodedSig))
	}
	writeInfo := storageTools("bundle-info", "set -eu\n"+rcloneRemote(bundleRemote)+fmt.Sprintf(`
cat <<'GENTIAN_INFO_EOF' | rclone rcat "%s:%s/%s/bundle-info.json"
%s
GENTIAN_INFO_EOF
echo "wrote bundle info"%s`, bundleRemote, p.Bucket, p.Prefix, string(encodedInfo), signature), bundleEnv(p))
	writeInfo.VolumeMounts = nil
	// Hardened like the containers uploadJob made: appended after it, this
	// one was not, and the tenant baseline policy refused the whole pod --
	// so no export ever finished, and no move ever started
	// (gentian-disallow-privilege-escalation, on the Job's second container).
	job.Spec.Template.Spec.Containers = hardened(append(job.Spec.Template.Spec.Containers, writeInfo))
	return job, nil
}
