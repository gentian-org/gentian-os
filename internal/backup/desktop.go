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

	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The desktop's database, where it is the kernel's.
//
// Every tenant's desktop keeps what its people set up -- layouts, a
// background, what they dismissed -- and the tenant's notices in a database
// of its own. For every tenant but one that database is on the tenants'
// PostgreSQL, made like an app's (DesktopStore), and is captured, restored
// and dropped like an app's, by the administrator of that server.
//
// The tenant that adopts the kernel realm keeps its desktop's database on
// the kernel's own PostgreSQL instead: the one database the kernel's chart
// declares for it there (kernel/data/kernel-postgres), beside the identity
// provider's and the rights store's. That server has no administrator's
// Secret, on purpose -- and a unit of a tenant's backup has no business
// holding one beside those databases. What is there is the credential of
// the database's own role, and it is enough: the owner of a database can
// dump it, replace its contents and empty it, and can do none of that to
// its neighbours. So the three Jobs here run in the kernel's data
// namespace as that role.
//
// The database is the tenant's whole: one desktop writes to it, and no
// other tenant's rows are in it. It is carried whole.

const (
	// KernelDesktopDatabase and KernelDesktopRole are the database and its
	// owner as the kernel's chart declares them.
	KernelDesktopDatabase = "portal_shell"
	KernelDesktopRole     = "portal_shell_user"
	// KernelDesktopSecret holds the role's credential, beside the server:
	// keys username and password.
	KernelDesktopSecret = "kernel-postgres-role-portal-shell"
	// KernelPostgresPort is the port the kernel's PostgreSQL answers on.
	KernelPostgresPort = "5432"
)

// KernelPostgresNamespace is where the kernel's PostgreSQL runs.
func KernelPostgresNamespace() string { return layout.Namespace(layout.Data) }

// KernelPostgresHost is the kernel's PostgreSQL: its read-write Service,
// the primary whichever instance that is.
func KernelPostgresHost() string {
	return fmt.Sprintf("kernel-postgres-rw.%s.svc.cluster.local", KernelPostgresNamespace())
}

// KernelDesktopDestroyJobName names the Job that empties the database.
func KernelDesktopDestroyJobName(tenantName string) string {
	return fmt.Sprintf("pg-empty-%s-%s", tenantName, DesktopStore)
}

// kernelDesktopEnv connects as the database's owner, to the database.
func kernelDesktopEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: KernelPostgresHost()},
		{Name: "PGPORT", Value: KernelPostgresPort},
		{Name: "PGDATABASE", Value: KernelDesktopDatabase},
		meta.SecretEnv("PGUSER", KernelDesktopSecret, "username"),
		meta.SecretEnv("PGPASSWORD", KernelDesktopSecret, "password"),
	}
}

// KernelDesktopDumpJob captures the desktop's database on the kernel's
// PostgreSQL, as its owner: the dump PostgresDumpJob takes, with another
// credential. It is filed like any database, under the database's name.
func KernelDesktopDumpJob(p JobParams) *batchv1.Job {
	return postgresDumpJob(p, KernelDesktopDatabase, kernelDesktopEnv())
}

// kernelDesktopOwned is the question both Jobs below ask before they
// change anything: is the database this connection is in the one of the
// role it connected as. A credential that opens another database, or opens
// this one as somebody who does not own it, is not the desktop's.
const kernelDesktopOwned = `owner="$(psql -v ON_ERROR_STOP=1 -tA <<'PSQL'
SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
 WHERE d.datname = current_database() AND r.rolname = current_user;
PSQL
)"
if [ -z "${owner}" ]; then
  echo "ERROR: refused: the database ${PGDATABASE} is not owned by the role this Job connects as. Nothing was changed" >&2; exit 1
fi
`

// KernelDesktopRestoreJob replaces the contents of the desktop's database on
// the kernel's PostgreSQL with a dump's, as its owner.
//
// As PostgresRestoreJob does, without what only an administrator can do and
// what the owner does not need: the objects are created by the role they
// are to belong to, so there is no ownership to hand over.
func KernelDesktopRestoreJob(p JobParams, d Decryption, artefact string) *batchv1.Job {
	restore := corev1.Container{
		Name:    "pg-restore",
		Image:   kernel.PostgresProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
%[1]s
# Replaced, not merged, and all of it or none: see PostgresRestoreJob.
pg_restore --clean --if-exists --no-owner --no-acl --single-transaction \
  --dbname="${PGDATABASE}" %[2]s/dump.pgc
echo "restored ${PGDATABASE}"`, kernelDesktopOwned, workDir)},
		Env:          kernelDesktopEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "dump.pgc"),
	}, restore, nil)
}

// kernelDesktopDestroyScript empties the desktop's database on the kernel's
// PostgreSQL.
//
// The database and its role are not dropped: the kernel's chart declares
// both, the cluster's own reconciliation would make them again, and the
// role this runs as could not drop itself. What is the tenant's is what is
// in the database, and that goes: everything the role owns in it. The
// script passes only when the server says nothing of the role's is left.
func kernelDesktopDestroyScript() string {
	return fmt.Sprintf(`set -eu
%[1]s
psql -v ON_ERROR_STOP=1 -c 'DROP OWNED BY CURRENT_USER CASCADE' >/dev/null
left="$(psql -v ON_ERROR_STOP=1 -tA <<'PSQL'
SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
 WHERE r.rolname = current_user AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f');
PSQL
)"
if [ "${left}" != "0" ]; then
  echo "ERROR: ${left} object(s) are still in ${PGDATABASE}" >&2; exit 1
fi
echo "the database ${PGDATABASE} is empty"
`, kernelDesktopOwned)
}

// KernelDesktopDestroyJob empties the desktop's database of the tenant that
// keeps it on the kernel's PostgreSQL: the Job that tenant's deletion runs.
func KernelDesktopDestroyJob(tenantName string, deadline DestroyDeadline) *batchv1.Job {
	return destroyJob(KernelPostgresNamespace(), KernelDesktopDestroyJobName(tenantName), tenantName, DesktopStore, deadline,
		corev1.Container{
			Name:    "empty-db",
			Image:   kernel.PostgresProvisionerImage(),
			Command: []string{"/bin/sh", "-c", kernelDesktopDestroyScript()},
			Env:     kernelDesktopEnv(),
		})
}
