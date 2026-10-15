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
	"regexp"
	"strings"

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
	remote := fmt.Sprintf("%s:%s/%s/%s%s", bundleRemote, p.Bucket, p.Prefix, artefact, EncryptedSuffix)
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

	// Nothing is fetched but the artefact: the storage tools carry rclone and
	// age. This step used to download MinIO's client at run time, from an
	// address that stopped answering when MinIO withdrew it -- and with it
	// every restore of every kind.
	script := fmt.Sprintf(`set -eu
%[1]s
%[7]s

rclone copyto "%[2]s" '%[3]s'
rclone cat "%[2]s.sha256" > /tmp/expected.sha256
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
		encryptBootstrap(Encryption{Mode: d.Mode}), remote, cipher, artefact, decrypt, plain, rcloneRemote(bundleRemote))

	container := storageTools("fetch", script, bundleEnv(p))
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
// claim of the tenant being restored into. A bundle is restored only into the
// tenant of its own name, and still the two differ wherever the tenant's
// database or bucket prefix (spec.isolation) is not the one it had where the
// bundle was taken -- on another cluster, say.

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
// server holds every tenant's. Where the provisioned database has the name
// it had in the bundle -- source and database are the same -- each keeps its
// name. Where it does not, the tenant's database prefix here is another than
// where the bundle was taken, and each is put back under the provisioned
// database here, as MariaDB's are: a name that began with the source's
// provisioned name begins with the target's instead, and any other name is
// prefixed with it.
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
: > /tmp/pairs
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
  printf '%%s=%%s\n' "${name}" "${target}" >> /tmp/pairs
  echo "restored ${name} as ${target}"
  n=$((n + 1))
done < %[3]s/owned/INDEX
echo "restored ${n} database(s) ${ROLE} owned besides ${DB}"
%[6]s

psql -v ON_ERROR_STOP=1 -tA -v app_role="${ROLE}" -v app_db="${DB}" -d postgres > /tmp/now <<'PSQL'
%[4]s
PSQL
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  if ! grep -qxF -- "${name}" /tmp/restored; then
    echo "NOTE: ${ROLE} owns ${name}, which the bundle does not hold; it was left as it is"
  fi
done < /tmp/now`, shellSingleQuote(p.restoreRole()), shellSingleQuote(database), workDir, postgresOwnedSQL, shellSingleQuote(source), reportOwnedNames)},
		Env:          PostgresAdminEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "owned.tar.gz"),
		unpack,
	}, restore, nil)
}

// OwnedNamesMessageLimit is how much of a restore Job's termination message
// the names of the owned databases it put back may take: Kubernetes keeps
// 4096 bytes of it.
const OwnedNamesMessageLimit = 4000

// reportOwnedNames is the end of a restore of owned databases: each one put
// back, as "<name in the bundle>=<name here>" on a line of its own, in the
// Job's termination message, for the operator to tell the app's
// post-restore commands. The names are not known before the Job runs. As
// many whole lines as the message holds; the Job's output names every one.
var reportOwnedNames = fmt.Sprintf(`REPORT="${TERMINATION_LOG:-/dev/termination-log}"
: > "${REPORT}"
size=0
while IFS= read -r pair; do
  len=$(printf '%%s\n' "${pair}" | wc -c)
  if [ $((size + len)) -gt %d ]; then
    echo "NOTE: not every name fits in what this Job reports; the output above names every one"
    break
  fi
  printf '%%s\n' "${pair}" >> "${REPORT}"
  size=$((size + len))
done < /tmp/pairs`, OwnedNamesMessageLimit)

// ParseOwnedNames reads what reportOwnedNames left: the pairs, in order. A
// line that is not one name, "=" and another is left out: a name with "=" in
// it cannot be told apart from the pair.
func ParseOwnedNames(message string) []string {
	var pairs []string
	for _, line := range strings.Split(message, "\n") {
		if strings.Count(line, "=") == 1 {
			pairs = append(pairs, line)
		}
	}
	return pairs
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
// provisioned database's -- which is the tenant's prefix and the app's, and
// so is another name where the tenant's prefix here is another. Each
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
: > /tmp/pairs
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
  printf '%%s=%%s\n' "${name}" "${target}" >> /tmp/pairs
  echo "restored ${name} as ${target}"
  n=$((n + 1))
done < %[3]s/owned/INDEX
echo "restored ${n} database(s) that were ${SRC}'s besides itself"
%[7]s

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
			mariadbOwnedSQL(database, user), mariadbClient, mariadbHeldByAnother("@db", user), reportOwnedNames)
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

// S3RestoreJob puts one app bucket back: the bucket, the identity the app
// reads it with, and its objects.
//
// provision is the container install provisions the bucket with
// (ObjectStorageProvisionContainer), holding the key pair the vault holds
// for the app. A restore used to make the bucket and fill it and leave it at
// that: into a cluster where the bucket's user had not been made, the app
// came back to a bucket it was not allowed to read. It runs before the
// objects are written, so a restore that cannot provision does not replace
// the bucket's contents either.
func S3RestoreJob(p JobParams, d Decryption, artefact, bucket string, provision corev1.Container) *batchv1.Job {
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
	// app's objects back into the cluster's own object store, which is a different
	// system with different credentials whenever the bundle is external.
	//
	// Sharing bundleEnv here sent both at the destination: the restore tried to
	// create the app's bucket in someone else's account — denied, since the
	// policy's credential is scoped to objects — and had it been permitted it
	// would have mirrored the tenant's objects into the bundle bucket instead
	// of restoring them.
	restore := storageTools("s3-restore", "set -eu\n"+rcloneRemote(platformRemote)+fmt.Sprintf(`
# The bucket and the key the app reads it with were made by the step before
# this one, with the code install uses.
# sync makes the bucket match the archive rather than merging into it: an
# object deleted before the backup must not reappear, and one created since must
# not survive a restore that claims to return the tenant to that point.
# --ignore-times writes every object of the archive, whatever the one there
# looks like: an object of the same size and time is not the same object.
rclone sync --ignore-times %[1]s/restore "%[3]s:%[2]s"
echo "restored bucket %[2]s"`, workDir, bucket, platformRemote), PlatformStorageEnv())
	job := restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "bucket.tar.gz"),
		unpack,
		provision,
	}, restore, nil)
	// The provision step administers the store with weed shell.
	ObjectStoreShellPod(&job.Spec.Template)
	return job
}

// VolumeRestoreJob unpacks one claim's archive back onto it.
//
// The claim is mounted read-write here, unlike during capture. That makes this
// the most destructive Job the platform runs, which is why the controller only
// creates it with the app already paused: unpacking over a running app's volume
// would race its own writes.
func VolumeRestoreJob(p JobParams, d Decryption, artefact, claim string) *batchv1.Job {
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

// RealmSource is where a realm capture was taken, as far as a realm import
// needs it: where the tenant's mail domain there is not the one it is
// restored into, both mail domains.
type RealmSource struct {
	// MailDomain is the tenant's mail domain where the capture was taken and
	// TargetMailDomain the one it is put back into. Set, both, only where
	// they differ: each person's address at MailDomain is put back at
	// TargetMailDomain under the same name, and a username that was that
	// address with it. A value that is not a domain's name moves nothing.
	MailDomain       string
	TargetMailDomain string
}

// mailDomainName is a domain's name, in lower case: what a mail domain a
// realm import rewrites addresses between may be.
var mailDomainName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// mailMove is the two mail domains a realm import moves addresses between,
// both "" when there is no move: they are the same, either is unknown, or
// either is not a domain's name.
func (s RealmSource) mailMove() (from, to string) {
	from, to = strings.ToLower(s.MailDomain), strings.ToLower(s.TargetMailDomain)
	if from == to || len(from) > 253 || len(to) > 253 || !mailDomainName.MatchString(from) || !mailDomainName.MatchString(to) {
		return "", ""
	}
	return from, to
}

// RealmImportJob puts a tenant's realm, its people and their groups back.
//
// Passwords are not in the bundle — Keycloak's partial-export omits them — so
// restored members are created without credentials and have to be sent through
// a reset. The import is written to say so rather than leave an operator to
// discover it from members who cannot sign in.
//
// People keep the ids they had (Keycloak's partialImport keeps an id it is
// given, POST /users does not), so an app that keys an account by the
// token's subject finds the same person after a restore or an import. A
// person who is already in the realm is left under the id they have.
//
// Groups and the realm's roles are brought back to what the bundle recorded:
// the bundle is always of the tenant restored into, so the platform's groups
// (gentian:tenant:<tenant>:…) are named as they are here.
//
// Clients are not put back over the ones here. Every client the realm has is
// left as it is, the platform's above all: it made them on this cluster, with
// this cluster's secrets and addresses, and the export holds each secret only
// masked. A client of the bundle that is not here is made, with a new secret.
//
// Where the tenant's mail domain is another than where the bundle was taken
// (RealmSource.MailDomain), each person's address at the old one comes back
// at the new one, under the same name, and a username that was that address
// with it: the mailboxes are put back into the new domain under the same
// names (MailboxRestoreJob), and a person signs in to theirs by the address.
// An address at any other domain is the person's own and is not touched.
//
// Nothing is skipped in silence. A person who cannot be created, a group
// that is not there, a membership that is refused: each is said, the import
// goes on to the end so that the output names all of them, and the Job
// fails. It used to pass over each and report the count of what worked.
func RealmImportJob(p JobParams, d Decryption, artefact, realm string, source RealmSource) *batchv1.Job {
	mailFrom, mailTo := source.mailMove()
	restore := corev1.Container{
		Name:    "realm-import",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{realmScriptPrelude + fmt.Sprintf(`REALM=%[1]s
MAIL_FROM=%[3]s
MAIL_TO=%[4]s
API="${KEYCLOAK_URL}/admin/realms/${REALM}"
tar xzf %[2]s/realm.tar.gz -C %[2]s
for f in realm.json users.ndjson memberships.ndjson; do
  [ -f "%[2]s/${f}" ] || { echo "ERROR: the realm archive holds no ${f}" >&2; exit 1; }
done

# The tenant's mail domain was MAIL_FROM where the bundle was taken and is
# MAIL_TO here. Each person's address at the old one comes back at the new
# one under the same name, and a username that was that address with it,
# before anything is read by a username: the memberships find each person by
# the name they come back under. An address at any other domain is the
# person's own. "@" belongs to the match, so a subdomain is not the domain.
if [ -n "${MAIL_FROM}" ] && [ -n "${MAIL_TO}" ]; then
  moved=$(jq -s --arg from "@${MAIL_FROM}" '[.[] | select((.email // "") | ascii_downcase | endswith($from))] | length' %[2]s/users.ndjson)
  jq -c --arg from "@${MAIL_FROM}" --arg to "@${MAIL_TO}" '
    if ((.email // "") | ascii_downcase | endswith($from)) then
      .email as $was
      | .email = (.email[0:((.email | length) - ($from | length))] + $to)
      | if ((.username // "") | ascii_downcase) == ($was | ascii_downcase) then .username = (.email | ascii_downcase) else . end
    else . end' %[2]s/users.ndjson > %[2]s/users.moved.ndjson
  mv %[2]s/users.moved.ndjson %[2]s/users.ndjson
  echo "the addresses at ${MAIL_FROM} are put back at ${MAIL_TO}, the tenant's mail domain here: ${moved} person(s)"
fi
new_token

# Clients first, and only those that are not here: SKIP leaves every client
# the realm has as it is, with its id, its secret, its addresses and its
# roles. The platform declares the clients it makes and has made them here
# already; Keycloak's OVERWRITE would delete each and make it anew from the
# bundle, under a new id, at the addresses the bundle was taken at, and with
# the secret the export masked ("**********") as its secret. A client that
# is not here is made from the bundle without any masked value, so Keycloak
# gives it a new secret. Its roles come with it; a client here keeps its own.
jq '{ifResourceExists:"SKIP",
     clients: [(.clients // [])[]
               | del(.secret, .registrationAccessToken)
               | if (.attributes | type) == "object"
                 then .attributes |= with_entries(select(.value != "**********"))
                 else . end],
     roles: {client: (.roles.client // {})}}' \
  %[2]s/realm.json > %[2]s/import.json
kc POST "${API}/partialImport" %[2]s/import.json >/dev/null

# Then groups and the realm's roles, with OVERWRITE: brought back to what the
# bundle recorded. Scoped to the realm's contents; the realm itself, which the
# platform provisions, is never recreated.
jq '{ifResourceExists:"OVERWRITE", groups:(.groups//[]), roles:{realm:(.roles.realm//[])}}' \
  %[2]s/realm.json > %[2]s/import.json
kc POST "${API}/partialImport" %[2]s/import.json >/dev/null
echo "imported realm configuration"

enc() { printf '%%s' "$1" | jq -sRr @uri; }
failed=0

# The realm's default role, which POST /users would grant and a partial
# import does not. It is read from this realm, so the import names no role
# that is not there: a partial import would make one up.
kc GET "${API}" > %[2]s/realm-now.json
default_role=$(jq -r '.defaultRole.name // empty' %[2]s/realm-now.json)

# People come back one at a time, each by a partial import of that one person
# under the id the bundle recorded: an app that keys an account by the token's
# subject finds its own person again, not a stranger. One at a time, so that a
# person the provider refuses is named and the others still come back. A
# person who is there already is left as they are, under the id they have;
# FAIL rather than SKIP, so that a clash on the e-mail address is said, not
# passed over. What describes the person is carried, with their links to
# other providers as before; no credential, role mapping or group the bundle
# might hold (a partial import would make up a role it does not find).
# A client's service account is the client's, made with it, and is not a
# person to create.
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
  have=$(jq -r '.[0].id // empty' %[2]s/found.json)
  was=$(printf '%%s' "${line}" | jq -r '.id // empty')
  if [ -n "${have}" ]; then
    if [ -n "${was}" ] && [ "${have}" != "${was}" ]; then
      echo "NOTE: ${username} is already in the realm under id ${have}, not ${was} as in the bundle, and is left as they are"
    fi
    continue
  fi
  # An id is kept only in the form Keycloak gives a person of its own (a
  # UUID): the id becomes the person's subject, which the rights store keys
  # people by, and a bundle is not signed. Any other id (a federated
  # person's f:<provider>:<id>) is left out, and the person gets a new one.
  if [ -n "${was}" ] && ! printf '%%s' "${was}" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'; then
    echo "NOTE: ${username} had the id ${was}, which is not one Keycloak gives a person of its own; they come back under a new id"
  fi
  printf '%%s' "${line}" \
    | jq --arg role "${default_role}" '{ifResourceExists: "FAIL",
        users: [{id, username, email, emailVerified, firstName, lastName, attributes, requiredActions, federatedIdentities}
                | if ((.id // "") | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")) then . else del(.id) end
                | with_entries(select(.value != null))
                + {enabled: true}
                + (if $role == "" then {} else {realmRoles: [$role]} end)]}' \
    > %[2]s/user.json
  if kc POST "${API}/partialImport" %[2]s/user.json >/dev/null; then
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
  printf '%%s' "${line}" | jq -r '.groups[]?.path | select(. != null and . != "")' > %[2]s/paths
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
			quotedRealm(realm), workDir, shellSingleQuote(mailFrom), shellSingleQuote(mailTo))},
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
