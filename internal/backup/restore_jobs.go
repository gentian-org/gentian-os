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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// IdentityEnvVar carries an age identity into a restore's decryption step.
const IdentityEnvVar = "GENTIAN_BUNDLE_IDENTITY"

// Decryption is what a restore needs to open a bundle.
//
// A recipient bundle needs the identity, which by design lives off the cluster
// with the recovery kit — so a restore has to be handed it rather than finding
// it. That is the cost of the escrow model working at all, and asking for it at
// restore time is the point where an operator proves they still have it.
type Decryption struct {
	Mode gentianov1alpha1.ExportEncryptionMode

	// SecretName holds either the passphrase or the age identity, in the Job's
	// own namespace. The controller stages it and removes it afterwards.
	SecretName string
	SecretKey  string
}

// Validate reports why a bundle cannot be opened.
func (d Decryption) Validate() error {
	if d.SecretName == "" {
		switch d.Mode {
		case gentianov1alpha1.ExportEncryptionPassphrase:
			return fmt.Errorf("this bundle is passphrase-encrypted: spec.decryption.passphraseSecretRef is required")
		default:
			return fmt.Errorf(
				"this bundle is encrypted to an age recipient: spec.decryption.identitySecretRef is required " +
					"(the identity is escrowed off-cluster with the recovery kit)")
		}
	}
	return nil
}

// fetchAndDecrypt produces the init container that pulls one artefact from the
// bundle, checks it against the checksum recorded beside it, and decrypts it.
//
// Verification happens before decryption and the Job fails on a mismatch. A
// truncated upload is indistinguishable from a complete one until something
// compares it against what was written, and restoring half a database over a
// live one is the worst outcome this system can produce.
func fetchAndDecrypt(d Decryption, p JobParams, artefact, localFile string) corev1.Container {
	remote := fmt.Sprintf("gentian/%s/%s/%s%s", p.Bucket, p.Prefix, artefact, EncryptedSuffix)
	cipher := workDir + "/" + localFile + EncryptedSuffix
	plain := workDir + "/" + localFile

	var decrypt string
	switch d.Mode {
	case gentianov1alpha1.ExportEncryptionPassphrase:
		decrypt = fmt.Sprintf(`printf '%%s\n' "${%s}" \
  | script -qec "age -d -o '%s' '%s'" /dev/null >/dev/null`, PassphraseEnvVar, plain, cipher)
	default:
		decrypt = fmt.Sprintf(`printf '%%s' "${%s}" > /tmp/identity
chmod 600 /tmp/identity
age -d -i /tmp/identity -o '%s' '%s'
rm -f /tmp/identity`, IdentityEnvVar, plain, cipher)
	}

	script := fmt.Sprintf(`set -eu
%s
# Deliberately NOT apk-installing "mc": on Alpine that package is Midnight
# Commander, which installs a binary of the same name, satisfies any "is mc
# present" check, and then fails on the first alias call with something
# unrecognisable. The MinIO client is fetched to its own path instead.
MCLI=/usr/local/bin/mcli
if [ ! -x "${MCLI}" ]; then
  wget -qO "${MCLI}" https://dl.min.io/client/mc/release/linux-amd64/mc \
    || { echo "ERROR: could not fetch the MinIO client" >&2; exit 1; }
  chmod +x "${MCLI}"
fi
"${MCLI}" alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"

"${MCLI}" cp "%[2]s" '%[3]s'
"${MCLI}" cat "%[2]s.sha256" > /tmp/expected.sha256
actual="$(sha256sum '%[3]s' | cut -d' ' -f1)"
expected="$(cat /tmp/expected.sha256 | tr -d '[:space:]')"
if [ "${actual}" != "${expected}" ]; then
  echo "ERROR: checksum mismatch for %[4]s (expected ${expected}, got ${actual})" >&2
  exit 1
fi

%[5]s
[ -s '%[6]s' ] || { echo "ERROR: decryption produced no output" >&2; exit 1; }
rm -f '%[3]s'
echo "fetched and decrypted %[4]s"`,
		encryptBootstrap(Encryption{Mode: d.Mode}), remote, cipher, artefact, decrypt, plain)

	container := corev1.Container{
		Name:         "fetch",
		Image:        kernel.KeycloakProvisionerImage(),
		Command:      []string{"/bin/sh", "-c"},
		Args:         []string{script},
		Env:          bundleEnv(p),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	if d.SecretName != "" {
		name := PassphraseEnvVar
		if d.Mode != gentianov1alpha1.ExportEncryptionPassphrase {
			name = IdentityEnvVar
		}
		container.Env = append(container.Env, secretEnv(name, d.SecretName, d.SecretKey))
	}
	return container
}

// PostgresRestoreJob loads one database back from its dump.
func PostgresRestoreJob(p JobParams, d Decryption, database string) *batchv1.Job {
	artefact := "postgres/" + database + ".pgc"
	restore := corev1.Container{
		Name:    "pg-restore",
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
ROLE=%[1]s
DB=%[2]s

# Ownership first. A restore that failed part way leaves objects owned by the
# admin that ran it, and the next attempt — running as the app, correctly —
# cannot drop what it does not own. The database then wedges at exactly the
# moment someone is trying to recover it, and only hand-written psql gets it
# back. Normalising here makes a retry self-healing instead. Extension-owned
# objects are left alone: they belong to the extension, not the app.
psql -v ON_ERROR_STOP=1 -v app_role="${ROLE}" -d "${DB}" <<'PSQL'
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT nspname FROM pg_namespace
            WHERE nspname NOT LIKE 'pg\_%%'
              AND nspname NOT IN ('information_schema', 'public')
  LOOP
    EXECUTE format('ALTER SCHEMA %%I OWNER TO %%I', r.nspname, :'app_role');
  END LOOP;

  FOR r IN SELECT c.relname, n.nspname, c.relkind
             FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname NOT LIKE 'pg\_%%'
              AND n.nspname <> 'information_schema'
              AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
              AND NOT EXISTS (SELECT 1 FROM pg_depend d
                               WHERE d.objid = c.oid AND d.deptype = 'e')
  LOOP
    EXECUTE format('ALTER %%s %%I.%%I OWNER TO %%I',
                   CASE r.relkind
                     WHEN 'S' THEN 'SEQUENCE'
                     WHEN 'v' THEN 'VIEW'
                     WHEN 'm' THEN 'MATERIALIZED VIEW'
                     ELSE 'TABLE'
                   END,
                   r.nspname, r.relname, :'app_role');
  END LOOP;
END $$;
PSQL
echo "ownership normalised to ${ROLE}"

# --clean --if-exists drops each object before recreating it, so a restore over
# a live database replaces its contents rather than merging into them. Merging
# is the silent-corruption case: rows the bundle does not contain would survive
# and look restored.
#
# --single-transaction makes the whole load atomic: a failure half way leaves
# the database as it was, not half-replaced.
# --role: the connection is the admin's, but the objects must belong to the
# app. Restoring without it left every table owned by the postgres superuser,
# and the app's first query after the restore was "permission denied for
# table oc_appconfig" — data perfectly restored, unreadable by its owner.
pg_restore --role="${ROLE}" --clean --if-exists --no-owner --no-acl --single-transaction \
  --dbname="${DB}" %[3]s/dump.pgc
echo "restored %[4]s"`, shellSingleQuote(PostgresRole(p.Tenant, p.App)), shellSingleQuote(database), workDir, database)},
		Env:          postgresAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "dump.pgc"),
	}, restore, nil)
}

// MariaDBRestoreJob loads one MariaDB database back.
func MariaDBRestoreJob(p JobParams, d Decryption, database string) *batchv1.Job {
	artefact := "mariadb/" + database + ".sql.gz"
	restore := corev1.Container{
		Name:    "mariadb-restore",
		Image:   kernel.MariaDBProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
DB=%s
# Database names are derived by the platform and always [A-Za-z0-9_], so they
# need no identifier quoting — but the SQL below interpolates the name, so this
# asserts the property rather than trusting it.
if ! printf '%%s' "${DB}" | grep -qE '^[A-Za-z0-9_]+$'; then
  echo "ERROR: refusing to restore into unsafe database name '${DB}'" >&2; exit 1
fi

MYSQL="mariadb --host=${MYSQL_HOST} --port=${MYSQL_TCP_PORT} --user=${MYSQL_ADMIN_USER}"

# Recreate rather than load into whatever is there: the dump is a complete
# picture, and rows it does not contain have no business surviving a restore
# that claims to return the database to that point.
$MYSQL -e "DROP DATABASE IF EXISTS ${DB}; CREATE DATABASE ${DB};"
gunzip -c %s/dump.sql.gz | $MYSQL "${DB}"
echo "restored ${DB}"`, shellSingleQuote(database), workDir)},
		Env:          mariadbAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "dump.sql.gz"),
	}, restore, nil)
}

// S3RestoreJob puts one app bucket's objects back.
func S3RestoreJob(p JobParams, d Decryption, bucket string) *batchv1.Job {
	artefact := "s3/" + bucket + ".tar.gz"
	// A separate container because the mc image has no tar.
	unpack := corev1.Container{
		Name:    "unpack-bucket",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mkdir -p %[1]s/restore
tar xzf %[1]s/bucket.tar.gz -C %[1]s/restore
echo "unpacked bucket archive"`, workDir)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	restore := corev1.Container{
		Name:    "s3-restore",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc mb --ignore-existing "gentian/%[2]s"
# --remove makes the bucket match the archive rather than merging into it: an
# object deleted before the backup must not reappear, and one created since must
# not survive a restore that claims to return the tenant to that point.
mc mirror --preserve --overwrite --remove %[1]s/restore "gentian/%[2]s"
echo "restored bucket %[2]s"`, workDir, bucket)},
		Env:          bundleEnv(p),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "bucket.tar.gz"),
		unpack,
	}, restore, nil)
}

// VolumeRestoreJob unpacks one claim's archive back onto it.
//
// The claim is mounted read-write here, unlike during capture. That makes this
// the most destructive Job the platform runs, which is why the controller only
// creates it with the app already paused: unpacking over a running app's volume
// would race its own writes.
func VolumeRestoreJob(p JobParams, d Decryption, claim string) *batchv1.Job {
	artefact := "volumes/" + claim + ".tar.gz"
	// Alpine, not the mc image: this container only untars, and the mc image
	// has no tar.
	restore := corev1.Container{
		Name:    "volume-restore",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
# Deliberately not deleting the target first: excludePaths mean the archive is
# not always a complete picture of the volume, and wiping data the profile chose
# not to capture would turn a documented omission into data loss.
tar xzf %s/volume.tar.gz -C /target
echo "restored volume %s"`, workDir, claim)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "target", MountPath: "/target"},
			{Name: "work", MountPath: workDir},
		},
	}
	target := corev1.Volume{
		Name: "target",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "volume.tar.gz"),
	}, restore, []corev1.Volume{target})
}

// RealmImportJob puts a tenant's realm, its people and their groups back.
//
// Passwords are not in the bundle — Keycloak's partial-export omits them — so
// restored members are created without credentials and have to be sent through
// a reset. The import is written to say so rather than leave an operator to
// discover it from members who cannot sign in.
func RealmImportJob(p JobParams, d Decryption, realm string) *batchv1.Job {
	restore := corev1.Container{
		Name:    "realm-import",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{keycloak.ProvisionerBootstrap + fmt.Sprintf(`set -eu
REALM=%[1]s
API="${KEYCLOAK_URL}/admin/realms/${REALM}"
tar xzf %[2]s/realm.tar.gz -C %[2]s

TOKEN=$(curl -sf --max-time 30 -X POST \
  "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | jq -r .access_token)
[ -n "${TOKEN}" ] && [ "${TOKEN}" != "null" ] || { echo "ERROR: no admin token" >&2; exit 1; }
AUTH="Authorization: Bearer ${TOKEN}"

# partialImport with OVERWRITE brings groups, roles and clients back to what the
# bundle recorded. It is scoped to the realm's contents and never recreates the
# realm itself, which the platform provisions.
jq '{ifResourceExists:"OVERWRITE", groups:(.groups//[]), roles:(.roles//{}), clients:(.clients//[])}' \
  %[2]s/realm.json > %[2]s/import.json
curl -sf --max-time 120 -X POST -H "${AUTH}" -H "Content-Type: application/json" \
  --data @%[2]s/import.json "${API}/partialImport" >/dev/null
echo "imported realm configuration"

# Users come back one at a time: partialImport does not carry them, and a user
# that already exists must not be duplicated.
restored=0
while read -r line; do
  username=$(printf '%%s' "${line}" | jq -r .username)
  [ -n "${username}" ] && [ "${username}" != "null" ] || continue
  existing=$(curl -sf --max-time 30 -H "${AUTH}" \
    "${API}/users?exact=true&username=$(printf '%%s' "${username}" | jq -sRr @uri)" | jq -r '.[0].id // empty')
  if [ -n "${existing}" ]; then
    continue
  fi
  printf '%%s' "${line}" \
    | jq 'del(.id, .createdTimestamp, .federationLink, .serviceAccountClientId) + {enabled:true}' \
    > %[2]s/user.json
  if curl -sf --max-time 30 -X POST -H "${AUTH}" -H "Content-Type: application/json" \
      --data @%[2]s/user.json "${API}/users" >/dev/null; then
    restored=$((restored + 1))
  fi
done < %[2]s/users.ndjson

# Group membership, which lives on the user rather than in the realm export.
while read -r line; do
  uid_old=$(printf '%%s' "${line}" | jq -r .userId)
  username=$(grep -F "\"id\":\"${uid_old}\"" %[2]s/users.ndjson | head -1 | jq -r .username 2>/dev/null || echo "")
  [ -n "${username}" ] || continue
  uid=$(curl -sf --max-time 30 -H "${AUTH}" \
    "${API}/users?exact=true&username=$(printf '%%s' "${username}" | jq -sRr @uri)" | jq -r '.[0].id // empty')
  [ -n "${uid}" ] || continue
  printf '%%s' "${line}" | jq -r '.groups[]?.path' | while read -r path; do
    [ -n "${path}" ] || continue
    gid=$(curl -sf --max-time 30 -H "${AUTH}" \
      "${API}/groups?search=$(printf '%%s' "${path##*/}" | jq -sRr @uri)" \
      | jq -r --arg p "${path}" '.. | objects | select(.path? == $p) | .id' | head -1)
    [ -n "${gid}" ] || continue
    curl -sf --max-time 30 -X PUT -H "${AUTH}" "${API}/users/${uid}/groups/${gid}" >/dev/null || true
  done
done < %[2]s/memberships.ndjson

echo "restored ${restored} user(s) WITHOUT credentials - they must be sent a password reset"`, quotedRealm(realm), workDir)},
		Env:          keycloakAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, "identity/realm.tar.gz", "realm.tar.gz"),
	}, restore, nil)
}

func quotedRealm(realm string) string { return shellSingleQuote(realm) }

// restoreJob assembles a fetch/decrypt init step and the loading container.
func restoreJob(p JobParams, initContainers []corev1.Container, main corev1.Container, extraVolumes []corev1.Volume) *batchv1.Job {
	return newJob(p, []corev1.Container{main}, initContainers, extraVolumes)
}
