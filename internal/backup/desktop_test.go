/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// The Jobs for the desktop's database on the kernel's PostgreSQL connect
// to what the kernel's chart declares, with the Secret the chart writes for
// the database's role. A chart that renames one leaves the Jobs connecting
// to nothing.
func TestTheKernelDesktopJobsNameWhatTheKernelDeclares(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "kernel", "data", "kernel-postgres", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	values := read("values.yaml")
	for _, want := range []string{"database: " + KernelDesktopDatabase, "role: " + KernelDesktopRole, "name: portal-shell"} {
		if !strings.Contains(values, want) {
			t.Errorf("the kernel's PostgreSQL chart does not declare %q", want)
		}
	}
	if !strings.HasSuffix(KernelDesktopSecret, "-role-portal-shell") || !strings.Contains(read(filepath.Join("templates", "databases.yaml")), "-role-") {
		t.Errorf("the role's Secret %s is not the one the chart writes per database", KernelDesktopSecret)
	}
	if KernelPostgresHost() != "kernel-postgres-rw.kernel-data.svc.cluster.local" {
		t.Errorf("host = %s", KernelPostgresHost())
	}
}

// Each of the three runs beside the kernel's PostgreSQL as the database's
// own role, and holds no administrator's credential: there is none there,
// and the identity provider's and the rights store's databases are on the
// same server.
func TestTheKernelDesktopJobsRunAsTheDatabasesOwner(t *testing.T) {
	p, d := mailParams()
	p.Namespace = KernelPostgresNamespace()
	jobs := map[string]struct {
		job       *batchv1.Job
		container string
	}{
		"dump":    {KernelDesktopDumpJob(p), "pg-dump"},
		"restore": {KernelDesktopRestoreJob(p, d, PostgresArtefact(KernelDesktopDatabase)), "pg-restore"},
		"destroy": {KernelDesktopDestroyJob("platform", DestroyInTheBackground), "empty-db"},
	}
	for act, c := range jobs {
		if c.job.Namespace != "kernel-data" {
			t.Errorf("%s runs in %s", act, c.job.Namespace)
		}
		container := containerByName(c.job, c.container)
		env := map[string]corev1.EnvVar{}
		for _, e := range container.Env {
			env[e.Name] = e
		}
		for _, key := range []string{"PGUSER", "PGPASSWORD"} {
			ref := env[key].ValueFrom
			if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != KernelDesktopSecret {
				t.Errorf("%s: %s is not read from the role's Secret", act, key)
			}
		}
		if env["PGHOST"].Value != KernelPostgresHost() || env["PGDATABASE"].Value != KernelDesktopDatabase {
			t.Errorf("%s connects to %s/%s", act, env["PGHOST"].Value, env["PGDATABASE"].Value)
		}
		script := strings.Join(append(container.Command, container.Args...), "\n")
		if strings.Contains(script, PostgresAdminSecret) || strings.Contains(script, "-d postgres") || strings.Contains(script, "--role") {
			t.Errorf("%s does something only the server's administrator can", act)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") || strings.Contains(line, "2>/dev/null") {
				t.Errorf("%s: a failure is discarded or hidden: %s", act, line)
			}
		}
		body := container.Command[len(container.Command)-1]
		if len(container.Args) > 0 {
			body = container.Args[0]
		}
		if out, err := exec.Command("sh", "-n", "-c", body).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", act, err, out)
		}
	}
	// The dump is the one every database is dumped by, filed like any.
	if got := containerByName(jobs["dump"].job, "pg-dump").Args[0]; got != containerByName(PostgresDumpJob(p, KernelDesktopDatabase), "pg-dump").Args[0] {
		t.Error("the kernel's desktop database is dumped by another script than a tenant's")
	}
	// A restore and a deletion change nothing in a database that is not the
	// role's own, and the deletion drops neither the database nor the role.
	for _, act := range []string{"restore", "destroy"} {
		c := containerByName(jobs[act].job, jobs[act].container)
		script := strings.Join(append(c.Command, c.Args...), "\n")
		if !strings.Contains(script, "d.datname = current_database() AND r.rolname = current_user") || !strings.Contains(script, "refused") {
			t.Errorf("%s does not ask whose the database is before it changes it", act)
		}
	}
	destroy := kernelDesktopDestroyScript()
	if strings.Contains(destroy, "DROP DATABASE") || strings.Contains(destroy, "DROP ROLE") {
		t.Error("the deletion drops what the kernel's chart declares")
	}
	if !strings.Contains(destroy, "DROP OWNED BY CURRENT_USER") || !strings.Contains(destroy, "are still in") {
		t.Error("the deletion does not empty the database and then look")
	}
	if job := jobs["destroy"].job; job.Name != "pg-empty-platform-shell" || job.Labels["gentianos.io/tenant"] != "platform" {
		t.Errorf("destroy job %s %v", job.Name, job.Labels)
	}
}
