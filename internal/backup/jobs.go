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
	// ScratchLimit bounds the emptyDir a dump is staged in. Zero leaves it
	// unbounded, which lets one large tenant fill a node's ephemeral storage.
	ScratchLimit string
	// BackoffLimit bounds retries. Export sets this low deliberately: the
	// shared Job waiter recreates a failed Job, so an unbounded capture would
	// retry a genuinely broken dump forever while holding an app paused.
	BackoffLimit int32
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
pg_dump --format=custom --no-owner --no-acl --dbname="%s" --file=%s/dump.pgc
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
  "%s" | gzip -c > %s/dump.sql.gz
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
		excludes.WriteString(fmt.Sprintf(" --exclude=%s", shellSingleQuote(pattern)))
	}
	archive := corev1.Container{
		Name:    "archive",
		Image:   mcImage,
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

// S3MirrorJob copies one app bucket into the bundle.
//
// Objects are mirrored as-is rather than re-encoded: they are already opaque,
// and preserving keys is what lets a restore put them back under the same
// names the app's database refers to.
func S3MirrorJob(p JobParams, sourceBucket string) *batchv1.Job {
	target := fmt.Sprintf("gentian/%s/%s/s3/%s", p.Bucket, p.Prefix, sourceBucket)
	mirror := corev1.Container{
		Name:    "mirror",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
# The bundle bucket is created here rather than during tenant provisioning.
# Gating a tenant's readiness on backup infrastructure would couple every
# install to a bucket only exports need; --ignore-existing makes this
# idempotent, and anonymous access is denied on every pass.
mc mb --ignore-existing "gentian/${BUNDLE_BUCKET}"
mc anonymous set none "gentian/${BUNDLE_BUCKET}"
# --preserve keeps object metadata; without it a restored object loses its
# content-type and apps that serve it directly start handing out octet-stream.
mc mirror --preserve --overwrite "gentian/%s" "%s"
echo "mirrored %s"`, sourceBucket, target, sourceBucket)},
		Env: bundleEnv(p),
	}
	return newJob(p, []corev1.Container{mirror}, nil, nil)
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
func uploadJob(p JobParams, localFile, artefact string, producers []corev1.Container, extraVolumes []corev1.Volume) *batchv1.Job {
	local := workDir + "/" + localFile
	target := fmt.Sprintf("gentian/%s/%s/%s", p.Bucket, p.Prefix, artefact)
	upload := corev1.Container{
		Name:    "upload",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
# The bundle bucket is created here rather than during tenant provisioning.
# Gating a tenant's readiness on backup infrastructure would couple every
# install to a bucket only exports need; --ignore-existing makes this
# idempotent, and anonymous access is denied on every pass.
mc mb --ignore-existing "gentian/${BUNDLE_BUCKET}"
mc anonymous set none "gentian/${BUNDLE_BUCKET}"
mc cp "%[1]s" "%[2]s"
# The checksum is written after the artefact and read before any restore, so a
# bundle whose upload was cut short fails verification instead of restoring
# quietly truncated data.
sha256sum "%[1]s" | cut -d' ' -f1 | mc pipe "%[2]s.sha256"
echo "uploaded %[3]s"`, local, target, artefact)},
		Env:          bundleEnv(p),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return newJob(p, []corev1.Container{upload}, producers, extraVolumes)
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
						ExportLabel:         p.Export,
					},
				},
				Spec: corev1.PodSpec{
					// Never Always: a capture is a one-shot, and a restarting
					// pod would re-dump into an artefact already uploaded.
					RestartPolicy:  corev1.RestartPolicyOnFailure,
					InitContainers: initContainers,
					Containers:     containers,
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

// bundleEnv gives a container the credentials to reach MinIO and the name of
// the bucket it must ensure exists before writing.
func bundleEnv(p JobParams) []corev1.EnvVar {
	return append(minioAdminEnv(), corev1.EnvVar{Name: "BUNDLE_BUCKET", Value: p.Bucket})
}

func minioAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("MINIO_ENDPOINT", MinIOAdminSecret, "endpoint"),
		secretEnv("MINIO_ACCESS_KEY", MinIOAdminSecret, "accessKey"),
		secretEnv("MINIO_SECRET_KEY", MinIOAdminSecret, "secretKey"),
	}
}

func postgresAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("PGHOST", PostgresAdminSecret, "host"),
		secretEnv("PGPORT", PostgresAdminSecret, "port"),
		secretEnv("PGUSER", PostgresAdminSecret, "username"),
		secretEnv("PGPASSWORD", PostgresAdminSecret, "password"),
	}
}

func keycloakAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("KEYCLOAK_URL", KeycloakAdminSecret, "url"),
		secretEnv("KEYCLOAK_ADMIN_USERNAME", KeycloakAdminSecret, "username"),
		secretEnv("KEYCLOAK_ADMIN_PASSWORD", KeycloakAdminSecret, "password"),
	}
}

func mariadbAdminEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("MYSQL_HOST", MariaDBAdminSecret, "host"),
		secretEnv("MYSQL_TCP_PORT", MariaDBAdminSecret, "port"),
		secretEnv("MYSQL_PWD", MariaDBAdminSecret, "password"),
		secretEnv("MYSQL_ADMIN_USER", MariaDBAdminSecret, "username"),
	}
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
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
