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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"
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
		container.Env = append(container.Env, meta.SecretEnv(name, d.SecretName, d.SecretKey))
	}
	return container
}

// Every restore Job below takes two names: artefact, the path in the bundle
// the manifest gives for what was captured, and the database, bucket or
// claim of the tenant being restored into. They are the same thing under two
// names only when the tenant is called what the one the bundle was taken of
// was; a restore used to take one name for both, and so could not find the
// artefacts of a bundle brought in under another name.

// PostgresRestoreJob loads one database back from its dump.
func PostgresRestoreJob(p JobParams, d Decryption, artefact, database string) *batchv1.Job {
	restore := corev1.Container{
		Name:    "pg-restore",
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
ROLE=%[1]s
DB=%[2]s

# Whose database this is, before anything in it is touched. The database a
# restore loads is the one provisioning made for this app, and provisioning
# made its role the owner. One that is not there, or is another role's, is
# not this app's: everything below would hand its objects to this role and
# replace its contents. Refused, with nothing changed.
owner="$(psql -v ON_ERROR_STOP=1 -tA -v db="${DB}" -d postgres <<'PSQL'
SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = :'db';
PSQL
)"
if [ -z "${owner}" ]; then
  echo "ERROR: database ${DB} is not there; it is made when the app is installed, and a restore does not make it" >&2; exit 1
fi
if [ "${owner}" != "${ROLE}" ]; then
  echo "ERROR: refused: database ${DB} is owned by ${owner}, not by ${ROLE}, the role this restore loads for. Nothing was changed" >&2; exit 1
fi

# Ownership first. A restore that failed part way leaves objects owned by the
# admin that ran it, and the next attempt — running as the app, correctly —
# cannot drop what it does not own. The database then wedges at exactly the
# moment someone is trying to recover it, and only hand-written psql gets it
# back. Normalising here makes a retry self-healing instead. Extension-owned
# objects are left alone: they belong to the extension, not the app, and so are
# dependent ones — a serial or identity sequence follows the owner of the table
# it belongs to and postgres refuses to reassign it on its own.
#
# Generated statements piped through \gexec rather than a PL/pgSQL block:
# psql substitutes :'var' only outside quoted literals, and a dollar-quoted
# body is a quoted literal — so the variable arrived at postgres as a literal
# colon. No procedural block also means no dollar-quoting to be eaten by
# Kubernetes container-arg expansion. Both failures came from the same
# instinct to reach for a DO block where a query would do.
psql -v ON_ERROR_STOP=1 -v app_role="${ROLE}" -d "${DB}" <<'PSQL'
SELECT format('ALTER SCHEMA %%I OWNER TO %%I', nspname, :'app_role')
  FROM pg_namespace
 WHERE nspname NOT LIKE 'pg\_%%'
   AND nspname NOT IN ('information_schema', 'public')
\gexec

SELECT format('ALTER %%s %%I.%%I OWNER TO %%I',
              CASE c.relkind
                WHEN 'S' THEN 'SEQUENCE'
                WHEN 'v' THEN 'VIEW'
                WHEN 'm' THEN 'MATERIALIZED VIEW'
                ELSE 'TABLE'
              END,
              n.nspname, c.relname, :'app_role')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname NOT LIKE 'pg\_%%'
   AND n.nspname <> 'information_schema'
   AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d
                    WHERE d.objid = c.oid
                      AND d.deptype IN ('a', 'i', 'e'))
\gexec
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
echo "restored %[4]s"`, shellSingleQuote(p.restoreRole()), shellSingleQuote(database), workDir, database)},
		Env:          PostgresAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "dump.pgc"),
	}, restore, nil)
}

// PostgresOwnedRestoreJob puts back the databases an app's role owned
// besides the provisioned one, from the archive PostgresOwnedDumpJob wrote.
//
// Under which name: a database's name is whatever the app chose, and one
// server holds every tenant's. Restored into the tenant the bundle was taken
// of -- source and database are the same name -- each keeps its name. Into
// another tenant it cannot: the name is taken, by the original, on the
// cluster the bundle came from. So there each is put back under the
// provisioned database of the tenant restored into, as MariaDB's are: a name
// that began with the source's provisioned name begins with the target's
// instead, and any other name is prefixed with it.
//
// Each is created if it is not there, owned by the app's role, and loaded
// the way the provisioned one is: its contents are replaced by the dump's.
// One that is there and is another role's is not this app's to replace, and
// fails the restore before anything of it is changed -- under any name,
// which is what keeps a restore out of another tenant's database whatever
// the naming did. A database the role owns now that the archive does not
// hold is left as it is -- the bundle says nothing about it, and a restore
// does not destroy what its bundle does not cover -- and is named in the
// Job's output.
func PostgresOwnedRestoreJob(p JobParams, d Decryption, artefact, source, database string) *batchv1.Job {
	unpack := corev1.Container{
		Name:    "unpack-owned",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mkdir -p %[1]s/owned
tar xzf %[1]s/owned.tar.gz -C %[1]s/owned
[ -f %[1]s/owned/INDEX ] || { echo "ERROR: the archive has no INDEX" >&2; exit 1; }
echo "unpacked the archive of owned databases"`, workDir)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	restore := corev1.Container{
		Name:    "pg-restore-owned",
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
ROLE=%[1]s
DB=%[2]s
SRC=%[5]s
: > /tmp/restored
n=0
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  [ -s "%[3]s/owned/${n}.pgc" ] || { echo "ERROR: the archive lists ${name} and holds no dump of it" >&2; exit 1; }
  if [ "${SRC}" = "${DB}" ]; then
    target="${name}"
  else
    case "${name}" in
      "${SRC}_"*) target="${DB}_${name#"${SRC}_"}" ;;
      *) target="${DB}_${name}" ;;
    esac
  fi
  # PostgreSQL cuts a longer name short and says so in a notice nobody reads.
  if [ "$(printf '%%s' "${target}" | wc -c)" -gt 63 ]; then
    echo "ERROR: ${name} would be restored as ${target}, which is longer than a database name may be" >&2; exit 1
  fi
  owner="$(psql -v ON_ERROR_STOP=1 -tA -v db="${target}" -d postgres <<'PSQL'
SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = :'db';
PSQL
)"
  if [ -n "${owner}" ] && [ "${owner}" != "${ROLE}" ]; then
    echo "ERROR: refused: ${name} would be restored as ${target}, which is a database ${owner} owns, not ${ROLE}. It was not changed" >&2; exit 1
  fi
  psql -v ON_ERROR_STOP=1 -v db="${target}" -v app_role="${ROLE}" -d postgres <<'PSQL'
SELECT format('CREATE DATABASE %%I OWNER %%I', :'db', :'app_role')
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db')
\gexec
PSQL
  # By PGDATABASE for the same reason the dump is: the name is the tenant's.
  # pg_restore connects only when it is given a database, so it writes the
  # script and psql applies it -- to a file first, not through a pipe, so
  # that a pg_restore that fails stops this script rather than handing psql
  # half a dump.
  # Neither reads its input: the loop's is the list of names still to do.
  pg_restore --role="${ROLE}" --clean --if-exists --no-owner --no-acl -f "%[3]s/owned/${n}.sql" "%[3]s/owned/${n}.pgc" </dev/null
  PGDATABASE="${target}" psql -v ON_ERROR_STOP=1 --single-transaction -q -f "%[3]s/owned/${n}.sql" </dev/null >/dev/null
  rm -f "%[3]s/owned/${n}.sql"
  printf '%%s\n' "${target}" >> /tmp/restored
  echo "restored ${name} as ${target}"
  n=$((n + 1))
done < %[3]s/owned/INDEX
echo "restored ${n} database(s) ${ROLE} owned besides ${DB}"

psql -v ON_ERROR_STOP=1 -tA -v app_role="${ROLE}" -v app_db="${DB}" -d postgres > /tmp/now <<'PSQL'
%[4]s
PSQL
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  if ! grep -qxF -- "${name}" /tmp/restored; then
    echo "NOTE: ${ROLE} owns ${name}, which the bundle does not hold; it was left as it is"
  fi
done < /tmp/now`, shellSingleQuote(p.restoreRole()), shellSingleQuote(database), workDir, postgresOwnedSQL, shellSingleQuote(source))},
		Env:          PostgresAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "owned.tar.gz"),
		unpack,
	}, restore, nil)
}

// MariaDBRestoreJob loads one MariaDB database back.
func MariaDBRestoreJob(p JobParams, d Decryption, artefact, database string) *batchv1.Job {
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
# Unpacked first and then loaded: in a pipe a truncated archive would end
# the load early and the exit status would be the client's.
gunzip %[2]s/dump.sql.gz
$MYSQL "${DB}" < %[2]s/dump.sql
echo "restored ${DB}"`, shellSingleQuote(database), workDir)},
		Env:          MariaDBAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "dump.sql.gz"),
	}, restore, nil)
}

// MariaDBOwnedRestoreJob puts back the databases that were an app's besides
// the provisioned one, from the archive MariaDBOwnedDumpJob wrote.
//
// A database is the app's by its name, and the name begins with the
// provisioned database's -- which is the tenant's and the app's, and so is
// another name where the bundle is restored into another tenant. Each
// database in the archive is therefore put back under the provisioned name
// here: source_reports of the bundle becomes database_reports. Under that
// name it is the app's here, within what its user may touch and what a
// purge drops.
//
// Each is created or, where it is there, replaced, as the provisioned one
// is. One whose name here is a database another account holds rights on is
// not the app's to replace, and fails the restore. A database that is the
// app's now and that the archive does not hold is left as it is -- the
// bundle says nothing about it -- and is named in the Job's output.
//
// The names are the app's own choice: each is handed to the server as hex
// and quoted by it, and reaches the client only as an argument.
func MariaDBOwnedRestoreJob(p JobParams, d Decryption, artefact, source, database, user string) *batchv1.Job {
	unpack := corev1.Container{
		Name:    "unpack-owned",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mkdir -p %[1]s/owned
tar xzf %[1]s/owned.tar.gz -C %[1]s/owned
[ -f %[1]s/owned/INDEX ] || { echo "ERROR: the archive has no INDEX" >&2; exit 1; }
echo "unpacked the archive of owned databases"`, workDir)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	script := mariadbRefusal(database, user)
	if script == "" {
		script = fmt.Sprintf(`set -euo pipefail
SRC=%[1]s
DB=%[2]s
MARIADB=(%[5]s)
: > /tmp/restored
n=0
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  [ -s "%[3]s/owned/${n}.sql.gz" ] || { echo "ERROR: the archive lists ${name} and holds no dump of it" >&2; exit 1; }
  case "${name}" in
    "${SRC}_"*) ;;
    *) echo "ERROR: the archive lists ${name}, which is not named as a database of ${SRC}" >&2; exit 1 ;;
  esac
  target="${DB}_${name#"${SRC}_"}"
  hex="$(printf '%%s' "${target}" | od -An -v -tx1 | tr -d ' \n')"
  {
  printf "SET @db = CONVERT(UNHEX('%%s') USING utf8mb4);\n" "${hex}"
  cat <<'SQL'
DELIMITER //
BEGIN NOT ATOMIC
  DECLARE why VARCHAR(512);
  IF %[6]s THEN
    SET why = CONCAT('refused: ', @db, ' is a database another account holds rights on');
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = why;
  END IF;
  EXECUTE IMMEDIATE CONCAT('DROP DATABASE IF EXISTS `+"`', REPLACE(@db, '`', '``'), '`"+`');
  EXECUTE IMMEDIATE CONCAT('CREATE DATABASE `+"`', REPLACE(@db, '`', '``'), '`"+`');
END//
SQL
  } | "${MARIADB[@]}"
  gunzip "%[3]s/owned/${n}.sql.gz"
  "${MARIADB[@]}" "${target}" < "%[3]s/owned/${n}.sql"
  rm -f "%[3]s/owned/${n}.sql"
  printf '%%s\n' "${target}" >> /tmp/restored
  echo "restored ${name} as ${target}"
  n=$((n + 1))
done < %[3]s/owned/INDEX
echo "restored ${n} database(s) that were ${SRC}'s besides itself"

"${MARIADB[@]}" -N -s --raw > /tmp/now <<'SQL'
%[4]s ORDER BY s.schema_name;
SQL
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  if ! grep -qxF -- "${name}" /tmp/restored; then
    echo "NOTE: ${name} is ${DB}'s and the bundle does not hold it; it was left as it is"
  fi
done < /tmp/now`,
			shellSingleQuote(source), shellSingleQuote(database), workDir,
			mariadbOwnedSQL(database, user), mariadbClient, mariadbHeldByAnother("@db", user))
	}
	restore := corev1.Container{
		Name:         "mariadb-restore-owned",
		Image:        kernel.MariaDBProvisionerImage(),
		Command:      []string{"/bin/bash", "-c"},
		Args:         []string{script},
		Env:          MariaDBAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "owned.tar.gz"),
		unpack,
	}, restore, nil)
}

// restoreRole is the role a restore gives the objects to.
func (p JobParams) restoreRole() string {
	if p.Role != "" {
		return p.Role
	}
	return PostgresRole(p.Tenant, p.App)
}

// S3RestoreJob puts one app bucket back: the bucket, the user and the policy
// the app reads it with, and its objects.
//
// provision is the container install provisions the bucket with
// (ObjectStorageProvisionContainer), holding the key pair the vault holds
// for the app. A restore used to make the bucket and fill it and leave it at
// that: into a cluster where the bucket's user had not been made, the app
// came back to a bucket it was not allowed to read. It runs before the
// objects are written, so a restore that cannot provision does not replace
// the bucket's contents either.
func S3RestoreJob(p JobParams, d Decryption, artefact, bucket string, provision corev1.Container) *batchv1.Job {
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
	// Platform storage, not the bundle's location. The init container above
	// fetched the archive from wherever the bundle lives; this one puts the
	// app's objects back into the cluster's own MinIO, which is a different
	// system with different credentials whenever the bundle is external.
	//
	// Sharing bundleEnv here sent both at the destination: the restore tried to
	// create the app's bucket in someone else's account — denied, since the
	// policy's credential is scoped to objects — and had it been permitted it
	// would have mirrored the tenant's objects into the bundle bucket instead
	// of restoring them.
	restore := corev1.Container{
		Name:    "s3-restore",
		Image:   mcImage,
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mc alias set platform "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
# The bucket, its user and its policy were made by the step before this one,
# with the code install uses.
# --remove makes the bucket match the archive rather than merging into it: an
# object deleted before the backup must not reappear, and one created since must
# not survive a restore that claims to return the tenant to that point.
mc mirror --preserve --overwrite --remove %[1]s/restore "platform/%[2]s"
echo "restored bucket %[2]s"`, workDir, bucket)},
		Env:          PlatformStorageEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "bucket.tar.gz"),
		unpack,
		provision,
	}, restore, nil)
}

// VolumeRestoreJob unpacks one claim's archive back onto it.
//
// The claim is mounted read-write here, unlike during capture. That makes this
// the most destructive Job the platform runs, which is why the controller only
// creates it with the app already paused: unpacking over a running app's volume
// would race its own writes.
func VolumeRestoreJob(p JobParams, d Decryption, artefact, claim string) *batchv1.Job {
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

// RealmSource is where a realm capture was taken: the tenant and its realm.
type RealmSource struct {
	Tenant string
	Realm  string
}

// RealmImportJob puts a tenant's realm, its people and their groups back.
//
// Passwords are not in the bundle — Keycloak's partial-export omits them — so
// restored members are created without credentials and have to be sent through
// a reset. The import is written to say so rather than leave an operator to
// discover it from members who cannot sign in.
//
// Into the tenant the bundle was taken of, groups, roles and clients are
// brought back to what the bundle recorded. Into a tenant of another name
// the bundle's names are the other tenant's:
//
//   - the platform's groups are named for their tenant
//     (gentian:tenant:<tenant>:…), and are put back under this tenant's
//     names, with the memberships. Left as they were, every person came back
//     a member of groups nothing here reads, with access to nothing;
//   - clients are not imported. They are named for the old tenant and carry
//     its addresses; this tenant's were made when its apps were installed.
//     With them go the client roles, and the groups' mappings to them, which
//     provisioning made here for this tenant's own clients;
//   - the old realm's default role is left out.
//
// Nothing is skipped in silence. A person who cannot be created, a group
// that is not there, a membership that is refused: each is said, the import
// goes on to the end so that the output names all of them, and the Job
// fails. It used to pass over each and report the count of what worked.
func RealmImportJob(p JobParams, d Decryption, artefact, realm string, source RealmSource) *batchv1.Job {
	if source.Tenant == "" {
		source.Tenant = p.Tenant
	}
	if source.Realm == "" {
		source.Realm = realm
	}
	restore := corev1.Container{
		Name:    "realm-import",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{realmScriptPrelude + fmt.Sprintf(`REALM=%[1]s
SRC=%[3]s
DST=%[4]s
SRC_REALM=%[5]s
API="${KEYCLOAK_URL}/admin/realms/${REALM}"
OLD="gentian:tenant:${SRC}:"
NEW="gentian:tenant:${DST}:"
tar xzf %[2]s/realm.tar.gz -C %[2]s
for f in realm.json users.ndjson memberships.ndjson; do
  [ -f "%[2]s/${f}" ] || { echo "ERROR: the realm archive holds no ${f}" >&2; exit 1; }
done
new_token

# partialImport with OVERWRITE brings groups, roles and clients back to what the
# bundle recorded. It is scoped to the realm's contents and never recreates the
# realm itself, which the platform provisions.
if [ "${SRC}" = "${DST}" ]; then
  jq '{ifResourceExists:"OVERWRITE", groups:(.groups//[]), roles:(.roles//{}), clients:(.clients//[])}' \
    %[2]s/realm.json > %[2]s/import.json
else
  jq --arg old "${OLD}" --arg new "${NEW}" --arg default "default-roles-${SRC_REALM}" '
    def renamed:
      if ((.name // "") | startswith($old)) then
        .name as $was | ($new + ($was | ltrimstr($old))) as $now
        | .name = $now
        | (.. | objects | select(has("path")) | .path) |=
            (if startswith("/" + $was) then "/" + $now + ltrimstr("/" + $was) else . end)
      else . end;
    {ifResourceExists: "OVERWRITE",
     groups: [(.groups // [])[] | renamed | del(.. | objects | .clientRoles?)],
     roles: {realm: [(.roles.realm // [])[] | select(.name != $default) | del(.composites.client?)]}}' \
    %[2]s/realm.json > %[2]s/import.json
  echo "the bundle is of tenant ${SRC}: its groups are put back under ${NEW}, its clients are not imported"
fi
kc POST "${API}/partialImport" %[2]s/import.json >/dev/null
echo "imported realm configuration"

enc() { printf '%%s' "$1" | jq -sRr @uri; }
failed=0

# Users come back one at a time: partialImport does not carry them, and a user
# that already exists must not be duplicated. A client's service account is
# the client's, made with it, and is not a person to create.
restored=0
n=0
while IFS= read -r line; do
  [ $((n %% 50)) -ne 0 ] || new_token
  n=$((n + 1))
  username=$(printf '%%s' "${line}" | jq -r '.username // empty')
  if [ -z "${username}" ]; then
    echo "ERROR: the bundle holds a person without a username" >&2; failed=$((failed + 1)); continue
  fi
  [ "$(printf '%%s' "${line}" | jq -r '.serviceAccountClientId // empty')" = "" ] || continue
  if ! kc GET "${API}/users?exact=true&username=$(enc "${username}")" > %[2]s/found.json; then
    failed=$((failed + 1)); continue
  fi
  [ -z "$(jq -r '.[0].id // empty' %[2]s/found.json)" ] || continue
  printf '%%s' "${line}" \
    | jq 'del(.id, .createdTimestamp, .federationLink, .serviceAccountClientId) + {enabled:true}' \
    > %[2]s/user.json
  if kc POST "${API}/users" %[2]s/user.json >/dev/null; then
    restored=$((restored + 1))
  else
    echo "ERROR: ${username} could not be created" >&2; failed=$((failed + 1))
  fi
done < %[2]s/users.ndjson

# Group membership, which lives on the user rather than in the realm export.
n=0
while IFS= read -r line; do
  [ $((n %% 50)) -ne 0 ] || new_token
  n=$((n + 1))
  uid_old=$(printf '%%s' "${line}" | jq -r .userId)
  username=$(jq -rs --arg id "${uid_old}" '[.[] | select(.id == $id and .serviceAccountClientId == null)][0].username // empty' %[2]s/users.ndjson)
  [ -n "${username}" ] || continue
  printf '%%s' "${line}" | jq -r --arg old "/${OLD}" --arg new "/${NEW}" \
    '.groups[]?.path | select(. != null and . != "") | if startswith($old) then $new + ltrimstr($old) else . end' > %[2]s/paths
  [ -s %[2]s/paths ] || continue
  if ! kc GET "${API}/users?exact=true&username=$(enc "${username}")" > %[2]s/found.json; then
    failed=$((failed + 1)); continue
  fi
  uid=$(jq -r '.[0].id // empty' %[2]s/found.json)
  if [ -z "${uid}" ]; then
    echo "ERROR: ${username} is not in the realm, so their memberships could not be put back" >&2; failed=$((failed + 1)); continue
  fi
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    segments=$(printf '%%s' "${path}" | jq -sRr 'ltrimstr("/") | split("/") | map(@uri) | join("/")')
    if ! kc GET "${API}/group-by-path/${segments}" > %[2]s/group.json; then
      echo "ERROR: ${username} was a member of ${path}, which is not in the realm" >&2; failed=$((failed + 1)); continue
    fi
    gid=$(jq -r '.id // empty' %[2]s/group.json)
    if [ -z "${gid}" ] || ! kc PUT "${API}/users/${uid}/groups/${gid}" >/dev/null; then
      echo "ERROR: ${username} could not be put back into ${path}" >&2; failed=$((failed + 1))
    fi
  done < %[2]s/paths
done < %[2]s/memberships.ndjson

if [ "${failed}" -ne 0 ]; then
  echo "ERROR: ${failed} person(s) or membership(s) could not be put back; each is named above. ${restored} person(s) were created" >&2
  exit 1
fi
echo "restored ${restored} user(s) WITHOUT credentials - they must be sent a password reset"`,
			quotedRealm(realm), workDir, shellSingleQuote(source.Tenant), shellSingleQuote(p.Tenant), shellSingleQuote(source.Realm))},
		Env:          keycloakAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "realm.tar.gz"),
	}, restore, nil)
}

func quotedRealm(realm string) string { return shellSingleQuote(realm) }

// restoreJob assembles a fetch/decrypt init step and the loading container.
func restoreJob(p JobParams, initContainers []corev1.Container, main corev1.Container, extraVolumes []corev1.Volume) *batchv1.Job {
	return newJob(p, []corev1.Container{main}, initContainers, extraVolumes)
}
