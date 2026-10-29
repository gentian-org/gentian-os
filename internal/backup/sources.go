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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// A bundle source: S3 storage outside the cluster that bundles are read from
// (BundleSource). Its keys are named, stored and materialized the way a
// backup destination's are: a CredentialRequirement derived from the name, a
// path under the kernel's subtree of the vault, and a Secret beside the
// object store, where the Jobs that read it run.

// SourceCredentialName is the CredentialRequirement carrying a source's keys,
// and the Secret ESO materializes them into beside the object store. Derived
// from the source's name, never configured, for the reason
// DestinationCredentialName gives.
func SourceCredentialName(source string) string { return "bundle-source-" + source }

// SourceVaultPath is where a source's keys live in OpenBao: under the
// kernel's subtree, which only the cluster's administrators write.
func SourceVaultPath(source string) string { return "gentian-os/kernel/backup/sources/" + source }

// SourceBundle is the bundle at prefix below a source, as every reader of a
// bundle takes it: the source's address, and the Secret its keys are in.
func SourceBundle(source *gentianov1alpha1.BundleSource, prefix string) gentianov1alpha1.BundleRef {
	return gentianov1alpha1.BundleRef{
		Bucket:           source.Spec.Bucket,
		Prefix:           JoinPrefix(source.Spec.Prefix, prefix),
		Endpoint:         source.Spec.Endpoint,
		Region:           source.Spec.Region,
		AddressingStyle:  source.Spec.AddressingStyle,
		CredentialSecret: SourceCredentialName(source.Name),
	}
}

// JoinPrefix is a bundle's prefix below a source's own, without a doubled or
// trailing slash.
func JoinPrefix(base, name string) string {
	base = strings.Trim(base, "/")
	name = strings.Trim(name, "/")
	if base == "" {
		return name
	}
	return base + "/" + name
}

// FetchLabel ties a fetch's Job, its pod and its way out together.
const FetchLabel = "gentianos.io/bundle-fetch"

// FetchComponent is the component label of a fetch's pod, which its egress
// policy selects by.
const FetchComponent = "bundle-fetch"

// FetchPolicyName is a fetch's way out of the cluster, by the fetch's name:
// gentian-fetch-source-<fetch>, beside the gentian-<export|restore>-destination-<run>
// of a run that writes or reads a destination.
func FetchPolicyName(fetch string) string { return "gentian-fetch-source-" + fetch }

// FetchJobTTLSeconds is how long a finished fetch is kept: long enough for
// whoever started it to read how it ended.
const FetchJobTTLSeconds int32 = 3600

// FetchParams is one fetch: a bundle at a place outside the cluster, copied
// into the import bucket of the cluster's own storage.
type FetchParams struct {
	// Namespace is the object store's, where the platform's keys and the
	// source's are.
	Namespace string
	// Name is the Job's, and the fetch's.
	Name string
	// From is the bundle where it is.
	From gentianov1alpha1.BundleRef
	// Bucket and Prefix are where it goes in the cluster's own storage.
	Bucket string
	Prefix string
	// Labels are put on the Job and its pod beside the fetch's own.
	Labels map[string]string
}

// BundleFetchJob copies a bundle from storage outside the cluster into the
// import bucket of the cluster's own, object by object, as it is: nothing is
// opened, the artifacts stay ciphertext and keep their checksums, which a
// restore verifies.
//
// One container with two remotes, the source's keys under SOURCE_* and the
// platform's under S3_*, so that rclone streams from one to the other with no
// scratch space. Its way out of the cluster is the fetch's own NetworkPolicy
// (netpolicy.BundleFetchNetworkPolicy).
func BundleFetchJob(p FetchParams) (*batchv1.Job, error) {
	if p.From.Endpoint != "" && p.From.CredentialSecret == "" {
		return nil, fmt.Errorf("a fetch from storage outside the cluster reads it with the keys declared for it")
	}
	if p.From.Bucket == "" || strings.Trim(p.From.Prefix, "/") == "" || p.Prefix == "" || p.Bucket == "" {
		return nil, fmt.Errorf("a fetch names the bundle's bucket and prefix, and where it goes")
	}
	env := PlatformStorageEnv()
	if p.From.Endpoint == "" {
		// A bucket of the cluster's own storage: read with its keys, from
		// inside.
		env = append(env,
			meta.SecretEnv("SOURCE_ENDPOINT", StorageAdminSecret, "endpoint"),
			meta.SecretEnv("SOURCE_ACCESS_KEY", StorageAdminSecret, "accessKey"),
			meta.SecretEnv("SOURCE_SECRET_KEY", StorageAdminSecret, "secretKey"))
	} else {
		env = append(env,
			corev1.EnvVar{Name: "SOURCE_ENDPOINT", Value: p.From.Endpoint},
			meta.SecretEnv("SOURCE_ACCESS_KEY", p.From.CredentialSecret, DestinationAccessKeyField),
			meta.SecretEnv("SOURCE_SECRET_KEY", p.From.CredentialSecret, DestinationSecretKeyField))
	}
	env = append(env,
		corev1.EnvVar{Name: "SOURCE_BUCKET", Value: p.From.Bucket},
		corev1.EnvVar{Name: "SOURCE_PREFIX", Value: strings.Trim(p.From.Prefix, "/")},
		corev1.EnvVar{Name: "IMPORT_BUCKET", Value: p.Bucket},
		corev1.EnvVar{Name: "IMPORT_PREFIX", Value: p.Prefix},
	)
	if p.From.Region != "" {
		env = append(env, corev1.EnvVar{Name: "SOURCE_REGION", Value: p.From.Region})
	}
	if p.From.AddressingStyle.VirtualHost() {
		env = append(env, corev1.EnvVar{Name: "SOURCE_ADDRESSING_STYLE", Value: string(p.From.AddressingStyle)})
	}
	script := "set -eu\n" + rcloneRemoteFrom("source", "SOURCE") + "\n" + rcloneRemote(platformRemote) + `
# The import bucket is the platform's, and made here when it is not there yet,
# as an upload makes it.
RCLONE_S3_NO_CHECK_BUCKET=false rclone mkdir "` + platformRemote + `:${IMPORT_BUCKET}"
# copy, not sync: the target prefix is new and nothing there is removed.
rclone copy --checksum "source:${SOURCE_BUCKET}/${SOURCE_PREFIX}/" "` + platformRemote + `:${IMPORT_BUCKET}/${IMPORT_PREFIX}/"
count="$(rclone lsf -R --files-only "` + platformRemote + `:${IMPORT_BUCKET}/${IMPORT_PREFIX}/" | wc -l)"
if [ "${count}" -eq 0 ]; then
  echo "ERROR: nothing under ${SOURCE_BUCKET}/${SOURCE_PREFIX} at the source" >&2
  exit 1
fi
echo "fetched ${count} file(s) of ${SOURCE_PREFIX}"`
	fetch := storageTools("fetch", script, env)
	fetch.VolumeMounts = nil

	labels := map[string]string{
		meta.ManagedByLabel: meta.ManagedByValue,
		meta.ComponentLabel: FetchComponent,
		FetchLabel:          p.Name,
	}
	for k, v := range p.Labels {
		labels[k] = v
	}
	ttl := FetchJobTTLSeconds
	backoff := int32(1)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: hardened([]corev1.Container{fetch}),
				},
			},
		},
	}, nil
}
