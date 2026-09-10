/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backup

import (
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ManifestSchemaVersion is bumped whenever the manifest's shape changes in a
// way a reader must notice. A restore refuses a version it does not know
// rather than guessing at fields that moved.
const ManifestSchemaVersion = 1

// Manifest is the index of a bundle, and the only part of it a restore reads
// before deciding whether it can proceed.
//
// It exists because a bundle outlives the cluster that produced it. Two years
// on, the question "what was running when this was taken, and can this platform
// still read it" has to be answerable from the bundle alone — not from a wiki,
// and not by unpacking a hundred gigabytes of dumps to find out.
type Manifest struct {
	SchemaVersion int `json:"schemaVersion"`

	// Tenant and TenantSpec snapshot the claim. A restore into a fresh cluster
	// re-applies the spec and lets the platform re-compose the tenant, so the
	// bundle never needs to carry provisioned infrastructure.
	Tenant     string                       `json:"tenant"`
	TenantSpec *gentianov1alpha1.TenantSpec `json:"tenantSpec,omitempty"`

	// Export names the TenantExport that produced this bundle.
	Export string `json:"export"`

	// CreatedAt is when the export started, RFC3339.
	CreatedAt string `json:"createdAt"`

	// OperatorVersion records the gentian-os build that wrote the bundle.
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// Apps records what was captured per app, including the pause window. The
	// windows are published rather than smoothed over: an export is consistent
	// within an app and not across them, and the timestamps are what make that
	// visible instead of merely documented.
	Apps []ManifestApp `json:"apps"`

	// Identity records the realm capture, when one was taken.
	Identity *ManifestIdentity `json:"identity,omitempty"`

	// Shell records the portal shell database, which belongs to the tenant
	// rather than to any app.
	Shell *ManifestStore `json:"shell,omitempty"`
}

// ManifestApp is one app's entry in the bundle index.
type ManifestApp struct {
	Name         string `json:"name"`
	Profile      string `json:"profile,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`

	// Stores lists what was captured, by kind.
	Stores []ManifestStore `json:"stores,omitempty"`

	// QuiesceStart and QuiesceEnd bound the window this app's writes were
	// paused, RFC3339. A restore reads them as the instant the app's data is
	// consistent as of.
	QuiesceStart string `json:"quiesceStart,omitempty"`
	QuiesceEnd   string `json:"quiesceEnd,omitempty"`

	// QuiesceMode records how writes were actually paused, which is not always
	// what the profile asked for — see the controller's fallback.
	QuiesceMode string `json:"quiesceMode,omitempty"`

	// BoundSecretKeys names the secrets carried for this app. Names only: the
	// values are in the bundle, and repeating them in an index that tooling
	// prints would defeat the point of encrypting it.
	BoundSecretKeys []string `json:"boundSecretKeys,omitempty"`
}

// ManifestStore is one captured artefact.
type ManifestStore struct {
	// Kind is postgres, mariadb, s3, volume or identity.
	Kind string `json:"kind"`
	// Name is the database, bucket or claim captured.
	Name string `json:"name"`
	// Path is the artefact's location within the bundle prefix.
	Path string `json:"path"`
}

// ManifestIdentity records the realm capture.
type ManifestIdentity struct {
	Realm string `json:"realm"`
	Path  string `json:"path"`
	// PasswordsIncluded is always false today, and is recorded rather than
	// assumed: a restore has to tell members their password will not come back,
	// and it should read that from the bundle rather than from a constant that
	// might change.
	PasswordsIncluded bool `json:"passwordsIncluded"`
}

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
