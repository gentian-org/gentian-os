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
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	// MinIOAdminSecret holds the credentials every capture uses to write into
	// the tenant's backup bucket. Captures authenticate as the MinIO admin
	// because they read app buckets they were never issued a key for; the
	// bundle they produce is encrypted, and no tenant workload can reach it.
	MinIOAdminSecret = "minio-admin"
	// PostgresAdminSecret and MariaDBAdminSecret hold the superuser credentials
	// the dumps run as. A per-app role can read only its own database, which is
	// exactly what a dump needs, but the roles are per app and the connection
	// details are not — using the admin secret keeps one code path.
	PostgresAdminSecret = "postgres-admin"
	MariaDBAdminSecret  = "mariadb-admin"
	// KeycloakAdminSecret holds the realm-export credentials, the same Secret
	// the identity reconciler already authenticates with.
	KeycloakAdminSecret = "keycloak-admin"

	// mcImage carries the MinIO client used to upload every artefact.
	mcImage = "minio/mc:RELEASE.2025-04-03T17-07-56Z"

	// workDir is the scratch mount a dump is staged in before upload.
	//
	// Staging rather than streaming is a deliberate v1 simplification: piping a
	// dump straight into `mc pipe` needs the dump tool and mc in one image, and
	// the platform has no such image yet. The cost is real — a capture needs
	// ephemeral node storage the size of the artefact — so ScratchLimit exists
	// to bound it, and streaming is the obvious Phase 6 follow-up.
	workDir = "/work"
)

// JobParams is the shared shape of every capture Job.
type JobParams struct {
	// Namespace is the kernel namespace the Job runs in.
	Namespace string
	// Name is the Job's own name; it must be unique per export and unit.
	Name string
	// Tenant and App label the Job so it is swept by the same machinery as
	// every other kernel Job. App is the component name for tenant-wide units
	// such as the portal shell database.
	Tenant string
	App    string
	// Export names the TenantExport this Job serves, for provenance.
	Export string
	// Bucket and Prefix locate the bundle this Job writes into.
	Bucket string
	Prefix string
	// Endpoint is the S3 API URL when bundles go somewhere other than the
	// platform's own MinIO, whose address travels with its credentials.
	// Resolved from BackupPolicy; empty means the platform's own storage.
	Endpoint string
	// Region is passed to S3 tooling that requires one. Recorded rather than
	// used by mc, which derives a bucket's location from the endpoint.
	Region string
	// ScratchLimit bounds the emptyDir a dump is staged in. Zero leaves it
	// unbounded, which lets one large tenant fill a node's ephemeral storage.
	ScratchLimit string
	// BackoffLimit bounds retries. Export sets this low deliberately: the
	// shared Job waiter recreates a failed Job, so an unbounded capture would
	// retry a genuinely broken dump forever while holding an app paused.
	BackoffLimit int32
	// Encryption protects every artefact this Job writes. There is no unset
	// value that means plaintext; Encryption.Validate rejects that before any
	// Job is built.
	Encryption Encryption
	// UploadCredentialsSecret overrides where the MinIO credentials come from.
	//
	// Empty means the kernel's minio-admin Secret, which is right for every
	// Job that runs in the kernel namespace. Volume Jobs cannot: a PVC is only
	// mountable from its own namespace, so they run in the tenant namespace —
	// where minio-admin does not exist and must not. The controller stages a
	// short-lived copy there and names it here.
	UploadCredentialsSecret string
}

// hardened gives every container the security context the tenant baseline
// admission policies require. Idempotent per container; an explicit context a
// producer already set is preserved and only the required fields are filled.
func hardened(containers []corev1.Container) []corev1.Container {
	no := false
	for i := range containers {
		sc := containers[i].SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		if sc.AllowPrivilegeEscalation == nil {
			sc.AllowPrivilegeEscalation = &no
		}
		if sc.SeccompProfile == nil {
			sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		}
		containers[i].SecurityContext = sc
	}
	return containers
}

// uploadSecretName resolves which Secret carries the MinIO credentials.
func (p JobParams) uploadSecretName() string {
	if p.UploadCredentialsSecret != "" {
		return p.UploadCredentialsSecret
	}
	return MinIOAdminSecret
}

// PostgresDumpJob captures one PostgreSQL database as a custom-format dump.
//
// Custom format (-Fc) rather than plain SQL: it is compressed, it is what
// pg_restore consumes, and it allows a selective restore of individual tables
// later without re-running the whole export.
func PostgresDumpJob(p JobParams, database string) *batchv1.Job {
	artefact := "postgres/" + database + ".pgc"
	dump := corev1.Container{
		Name:    "pg-dump",
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
# --no-owner / --no-acl: roles are re-derived on restore, so recording the
# owner would only pin the dump to this cluster's role names.
pg_dump --format=custom --no-owner --no-acl --dbname=%s --file=%s/dump.pgc
echo "dumped %s"`, shellSingleQuote(database), workDir, database)},
		Env:          postgresAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return uploadJob(p, "dump.pgc", artefact, []corev1.Container{dump}, nil)
}

// MariaDBDumpJob captures one MariaDB database as compressed SQL.
func MariaDBDumpJob(p JobParams, database string) *batchv1.Job {
	artefact := "mariadb/" + database + ".sql.gz"
	dump := corev1.Container{
		Name:    "mariadb-dump",
		Image:   kernel.MariaDBProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
# --single-transaction takes a consistent snapshot without locking the whole
# database; the app is already paused, so this only guards in-flight work.
mariadb-dump --single-transaction --routines --triggers --events \
  --host="${MYSQL_HOST}" --port="${MYSQL_TCP_PORT}" --user="${MYSQL_ADMIN_USER}" \
  %s | gzip -c > %s/dump.sql.gz
echo "dumped %s"`, shellSingleQuote(database), workDir, database)},
		Env:          mariadbAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return uploadJob(p, "dump.sql.gz", artefact, []corev1.Container{dump}, nil)
}

// VolumeArchiveJob captures one PersistentVolumeClaim as a compressed archive.
//
// The claim is mounted read-only. Excludes come from the profile's
// spec.backup.volumes.excludePaths and are the app author's statement about
// which bytes are derived; they are applied as tar patterns, so a pattern that
// matched a config directory would silently drop the key an app's data is
// encrypted with — which is why the catalogue rejects such patterns rather
// than trusting this layer to notice.
func VolumeArchiveJob(p JobParams, claim string, excludePaths []string) *batchv1.Job {
	artefact := "volumes/" + claim + ".tar.gz"
	var excludes strings.Builder
	for _, pattern := range sortedUnique(excludePaths) {
		fmt.Fprintf(&excludes, " --exclude=%s", shellSingleQuote(pattern))
	}
	// Alpine, not the mc image: this container only tars, and the mc image —
	// a minimal UBI — ships no tar at all. Every container that runs tar must
	// use an image that has it; the first live volume capture exited 127 on
	// its first command.
	archive := corev1.Container{
		Name:    "archive",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
tar czf %s/volume.tar.gz -C /source%s .
echo "archived %s"`, workDir, excludes.String(), claim)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "source", MountPath: "/source", ReadOnly: true},
			{Name: "work", MountPath: workDir},
		},
	}
	source := corev1.Volume{
		Name: "source",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: claim,
				ReadOnly:  true,
			},
		},
	}
	return uploadJob(p, "volume.tar.gz", artefact, []corev1.Container{archive}, []corev1.Volume{source})
}

// S3ArchiveJob captures one app bucket into the bundle.
//
// It copies the bucket to disk and archives it rather than mirroring object to
// object. Mirroring was the obvious shape and the wrong one: bundle artefacts
// are encrypted individually, and a mirrored bucket would have landed as
// plaintext objects — leaving the largest part of most tenants' data readable
// by anyone who could list the bundle, in a bundle whose whole premise is that
// it is encrypted. The cost is staging space bounded by ScratchLimit.
//
// Object keys are preserved inside the archive, which is what lets a restore
// put each object back under the name the app's database refers to.
func S3ArchiveJob(p JobParams, sourceBucket string) *batchv1.Job {
	artefact := "s3/" + sourceBucket + ".tar.gz"
	// The app's bucket is in the platform's own MinIO, always — it is where the
	// app writes. The bundle may be somewhere else entirely, and the two are the
	// same system only when bundles go to platform storage.
	//
	// Sharing bundleEnv here pointed the source read at the destination: with an
	// external bundle store this went looking for the tenant's bucket in someone
	// else's account, and failed in fetch-bucket before a byte was captured. It
	// worked for as long as it did because every bundle went to the same MinIO
	// the apps use, which made one set of credentials look like enough for both.
	// The restore path already carries this distinction; capture did not.
	fetch := corev1.Container{
		Name:    "fetch-bucket",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set platform "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mkdir -p %[1]s/bucket
# An empty bucket is normal (an app may never have written), so mirror into a
# directory that already exists and let the pack step archive it empty too.
mc mirror --preserve "platform/%[2]s" %[1]s/bucket
echo "fetched bucket %[2]s"`, workDir, sourceBucket)},
		Env:          PlatformStorageEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	// A separate container because the mc image has no tar.
	pack := corev1.Container{
		Name:    "pack-bucket",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
tar czf %[1]s/bucket.tar.gz -C %[1]s/bucket .
rm -rf %[1]s/bucket
echo "archived bucket %[2]s"`, workDir, sourceBucket)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return uploadJob(p, "bucket.tar.gz", artefact, []corev1.Container{fetch, pack}, nil)
}

// RealmExportJob captures a tenant's Keycloak realm: its configuration, and
// separately its people.
//
// Keycloak's partial-export returns the realm, its clients, groups and roles —
// and deliberately no users at all. A realm restored from partial-export alone
// is a correctly configured workspace with nobody in it, which is why this also
// pages through the users endpoint and records each user's group membership.
//
// Passwords are not captured. Partial-export omits credentials by design, and
// the whole-realm export that includes hashes is an offline operation against
// the running instance. So a restored realm re-invites its members rather than
// silently resurrecting an old password — stated here because it is a product
// decision hiding in an API limitation, not an oversight.
func RealmExportJob(p JobParams, realm string) *batchv1.Job {
	quoted := shellSingleQuote(realm)
	export := corev1.Container{
		Name:    "realm-export",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{keycloak.ProvisionerBootstrap + fmt.Sprintf(`set -eu
REALM=%[1]s
API="${KEYCLOAK_URL}/admin/realms/${REALM}"

token() {
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
    | jq -r .access_token
}
TOKEN=$(token)
[ -n "${TOKEN}" ] && [ "${TOKEN}" != "null" ] || { echo "ERROR: no admin token" >&2; exit 1; }
AUTH="Authorization: Bearer ${TOKEN}"

curl -sf --max-time 120 -X POST -H "${AUTH}" -H "Content-Type: application/json" \
  "${API}/partial-export?exportGroupsAndRoles=true&exportClients=true" > %[2]s/realm.json

# Users are paged: Keycloak caps a single response, so a realm larger than one
# page would otherwise be captured half-empty with no error anywhere.
first=0
page_size=100
: > %[2]s/users.ndjson
while : ; do
  page=$(curl -sf --max-time 60 -H "${AUTH}" \
    "${API}/users?briefRepresentation=false&first=${first}&max=${page_size}")
  count=$(printf '%%s' "${page}" | jq 'length')
  [ "${count}" -eq 0 ] && break
  printf '%%s' "${page}" | jq -c '.[]' >> %[2]s/users.ndjson
  first=$((first + page_size))
  [ "${count}" -lt "${page_size}" ] && break
done

# Group membership lives on the user, not in the realm export, so it is
# collected per user. Restoring people without their groups would restore
# accounts that can sign in and reach nothing.
: > %[2]s/memberships.ndjson
while read -r line; do
  uid=$(printf '%%s' "${line}" | jq -r .id)
  groups=$(curl -sf --max-time 30 -H "${AUTH}" "${API}/users/${uid}/groups" || echo '[]')
  jq -cn --arg id "${uid}" --argjson g "${groups}" '{userId:$id, groups:$g}' >> %[2]s/memberships.ndjson
done < %[2]s/users.ndjson

tar czf %[2]s/realm.tar.gz -C %[2]s realm.json users.ndjson memberships.ndjson
echo "exported realm ${REALM} ($(wc -l < %[2]s/users.ndjson) users)"`, quoted, workDir)},
		Env:          keycloakAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return uploadJob(p, "realm.tar.gz", "identity/realm.tar.gz", []corev1.Container{export}, nil)
}

// uploadJob wires a producing container to an uploader that puts the artefact
// in the bundle and records its checksum next to it.
//
// localFile and artefact are separate on purpose: producers write a fixed
// filename into the scratch mount, while the artefact carries the database or
// claim name it should be filed under in the bundle. Deriving one from the
// other looks tidy and silently uploads nothing.
//
// The producer runs as an init container so a failed dump never reaches the
// upload step: a Job whose dump failed but whose upload succeeded would leave a
// truncated artefact in the bundle and report success, which is the one failure
// mode a backup must never have.
// bucketPreparation returns the commands that ready the destination bucket,
// which differ by who owns it.
//
// The platform's own MinIO is administered by this operator with the kernel's
// admin credential, and the bundle bucket may not exist yet — creating it here
// is what keeps tenant provisioning from depending on backup infrastructure.
//
// An external destination is the opposite on every count. The bucket was
// created by whoever owns the account, and the credential the policy carries is
// deliberately scoped to objects, so create-bucket and put-bucket-policy are
// denied. mc's --ignore-existing does not cover a denial; it covers a bucket
// you already own. Issuing these against someone else's bucket failed on the
// upload container's first command, before a single artefact was sent:
//
//	mc: <ERROR> Unable to make bucket gentian/<bucket>.
//	    You are not authorized to perform create-bucket on bucket <bucket>
//
// A bucket that genuinely is not there still fails, on the mc cp below, with
// the S3 error that says so. Prechecking instead would mean asking for a
// listing permission we have equally little right to assume.
func bucketPreparation(p JobParams) string {
	if p.Endpoint != "" {
		return "# External destination: the bucket is not ours to create or configure."
	}
	return `# The bundle bucket is created here rather than during tenant provisioning.
# Gating a tenant's readiness on backup infrastructure would couple every
# install to a bucket only exports need; --ignore-existing makes this
# idempotent, and anonymous access is denied on every pass.
mc mb --ignore-existing "gentian/${BUNDLE_BUCKET}"
mc anonymous set none "gentian/${BUNDLE_BUCKET}"`
}

func uploadJob(p JobParams, localFile, artefact string, producers []corev1.Container, extraVolumes []corev1.Volume) *batchv1.Job {
	// Encryption runs as the last init container, so the uploader below can
	// only ever see ciphertext: the plaintext is removed before it starts.
	producers = append(producers, encryptContainer(p.Encryption, localFile))
	local := workDir + "/" + localFile + EncryptedSuffix
	target := fmt.Sprintf("gentian/%s/%s/%s%s", p.Bucket, p.Prefix, artefact, EncryptedSuffix)
	upload := corev1.Container{
		Name:    "upload",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
%[4]s
mc cp "%[1]s" "%[2]s"
# The checksum is written after the artefact and read before any restore, so a
# bundle whose upload was cut short fails verification instead of restoring
# quietly truncated data.
sha256sum "%[1]s" | cut -d' ' -f1 | mc pipe "%[2]s.sha256"
echo "uploaded %[3]s"`, local, target, artefact, bucketPreparation(p))},
		Env:          bundleEnv(p),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return newJob(p, []corev1.Container{upload}, producers, extraVolumes)
}

// BundleDeleteJob removes a bundle's objects from the backup bucket.
//
// It deletes the export's prefix, never the bucket: the bucket holds every
// bundle the tenant has, and deleting one backup must not be able to take the
// rest with it. An empty prefix is refused for the same reason — rm
// --recursive on the bare bucket is exactly the command the guard exists to
// make unwritable.
func BundleDeleteJob(p JobParams) *batchv1.Job {
	del := corev1.Container{
		Name:    "bundle-delete",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{`set -eu
[ -n "${BUNDLE_PREFIX}" ] || { echo "refusing to delete: empty bundle prefix" >&2; exit 1; }
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
# Nothing there is success, not failure: the bundle may never have been
# written (a capture that failed before its first upload), the bucket may not
# exist yet, or a previous attempt already removed the objects.
if ! mc stat "gentian/${BUNDLE_BUCKET}/${BUNDLE_PREFIX}/" >/dev/null 2>&1; then
  echo "no objects under ${BUNDLE_BUCKET}/${BUNDLE_PREFIX} - nothing to delete"
  exit 0
fi
mc rm --recursive --force "gentian/${BUNDLE_BUCKET}/${BUNDLE_PREFIX}/"
echo "deleted bundle ${BUNDLE_BUCKET}/${BUNDLE_PREFIX}"`},
		Env: append(bundleEnv(p), corev1.EnvVar{Name: "BUNDLE_PREFIX", Value: p.Prefix}),
	}
	return newJob(p, []corev1.Container{del}, nil, nil)
}

func newJob(p JobParams, containers, initContainers []corev1.Container, extraVolumes []corev1.Volume) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	backoff := p.BackoffLimit

	volumes := extraVolumes
	if needsWorkDir(containers, initContainers) {
		work := corev1.EmptyDirVolumeSource{}
		if p.ScratchLimit != "" {
			if q, err := resource.ParseQuantity(p.ScratchLimit); err == nil {
				work.SizeLimit = &q
			}
		}
		volumes = append(volumes, corev1.Volume{
			Name:         "work",
			VolumeSource: corev1.VolumeSource{EmptyDir: &work},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels: map[string]string{
				meta.TenantLabel:    p.Tenant,
				meta.AppLabel:       p.App,
				meta.ManagedByLabel: meta.ManagedByValue,
				meta.ComponentLabel: "tenant-export",
				ExportLabel:         p.Export,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						meta.TenantLabel:    p.Tenant,
						meta.ManagedByLabel: meta.ManagedByValue,
						// On the pod, not only the Job: the tenant-namespace
						// NetworkPolicy that lets a capture pod reach MinIO
						// selects on this.
						meta.ComponentLabel: "tenant-export",
						ExportLabel:         p.Export,
					},
				},
				Spec: corev1.PodSpec{
					// Never Always: a capture is a one-shot, and a restarting
					// pod would re-dump into an artefact already uploaded.
					RestartPolicy: corev1.RestartPolicyOnFailure,
					// Volume Jobs run in tenant namespaces, where Kyverno
					// enforces the pod-security baseline that platform-kernel
					// does not — the first tenant-namespace capture pod was
					// denied at admission for exactly these two settings.
					// Root without escalation is deliberate: an archive must
					// read app files whatever uid owns them, and neither
					// policy requires non-root.
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: hardened(initContainers),
					Containers:     hardened(containers),
					Volumes:        volumes,
				},
			},
		},
	}
}

// ExportLabel ties every Job, and the pod it creates, back to the TenantExport
// that asked for it — which is how a controller restart finds work already in
// flight instead of starting it again.
const ExportLabel = "gentianos.io/tenant-export"

func needsWorkDir(groups ...[]corev1.Container) bool {
	for _, group := range groups {
		for _, c := range group {
			for _, m := range c.VolumeMounts {
				if m.Name == "work" {
					return true
				}
			}
		}
	}
	return false
}

// bundleEnv gives a container the credentials to reach the bundle store and
// the name of the bucket it must ensure exists before writing.
//
// The endpoint comes from a different place depending on where bundles go, and
// that asymmetry is the point. The platform's own MinIO records its address
// alongside its keys, so all three read from one Secret. A destination the
// operator configured has its address in the policy — a fact, not a
// credential — while only the keys come from the credential manager. Reading
// the endpoint from the credential Secret in that case would mean an admin
// could change where every tenant's backups go by editing a secret.
func bundleEnv(p JobParams) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "BUNDLE_BUCKET", Value: p.Bucket}}

	if p.Endpoint == "" {
		secret := p.uploadSecretName()
		env = append(env,
			meta.SecretEnv("MINIO_ENDPOINT", secret, "endpoint"),
			meta.SecretEnv("MINIO_ACCESS_KEY", secret, "accessKey"),
			meta.SecretEnv("MINIO_SECRET_KEY", secret, "secretKey"),
		)
		return env
	}

	secret := p.uploadSecretName()
	env = append(env,
		corev1.EnvVar{Name: "MINIO_ENDPOINT", Value: p.Endpoint},
		meta.SecretEnv("MINIO_ACCESS_KEY", secret, DestinationAccessKeyField),
		meta.SecretEnv("MINIO_SECRET_KEY", secret, DestinationSecretKeyField),
	)
	if p.Region != "" {
		// mc derives a bucket's location from the endpoint, so this changes
		// nothing for the capture path. It is set because every other S3 tool
		// reads it, and a destination that works for mc and fails for the next
		// thing pointed at it would be a trap.
		env = append(env, corev1.EnvVar{Name: "AWS_REGION", Value: p.Region})
	}
	return env
}

// PlatformStorageEnv addresses the platform's own MinIO, whose address travels
// with its credentials.
//
// Distinct from bundleEnv, which addresses wherever the bundle lives. The two
// are the same place only when bundles go to platform storage; with an external
// destination they are different systems with different credentials, and a Job
// that needs both must not assume one covers the other.
func PlatformStorageEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("MINIO_ENDPOINT", MinIOAdminSecret, "endpoint"),
		meta.SecretEnv("MINIO_ACCESS_KEY", MinIOAdminSecret, "accessKey"),
		meta.SecretEnv("MINIO_SECRET_KEY", MinIOAdminSecret, "secretKey"),
	}
}

func postgresAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("PGHOST", PostgresAdminSecret, "host"),
		meta.SecretEnv("PGPORT", PostgresAdminSecret, "port"),
		meta.SecretEnv("PGUSER", PostgresAdminSecret, "username"),
		meta.SecretEnv("PGPASSWORD", PostgresAdminSecret, "password"),
	}
}

func keycloakAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("KEYCLOAK_URL", KeycloakAdminSecret, "url"),
		meta.SecretEnv("KEYCLOAK_ADMIN_USERNAME", KeycloakAdminSecret, "username"),
		meta.SecretEnv("KEYCLOAK_ADMIN_PASSWORD", KeycloakAdminSecret, "password"),
	}
}

func mariadbAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		meta.SecretEnv("MYSQL_HOST", MariaDBAdminSecret, "host"),
		meta.SecretEnv("MYSQL_TCP_PORT", MariaDBAdminSecret, "port"),
		meta.SecretEnv("MYSQL_PWD", MariaDBAdminSecret, "password"),
		meta.SecretEnv("MYSQL_ADMIN_USER", MariaDBAdminSecret, "username"),
	}
}

// shellSingleQuote renders a value as a single-quoted shell word.
//
// Every name reaching these scripts is derived by this package from a Tenant
// name and a profile name, both constrained by the API server, so none of them
// can currently contain a quote. That is a property of today's callers rather
// than of the scripts, and these scripts run as database and storage
// superusers, so they do not rely on it.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
