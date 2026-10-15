/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"slices"
	"strings"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// RestoreEnvPrefix begins the name of every variable a profile's
// post-restore commands are run with.
const RestoreEnvPrefix = "GENTIAN_RESTORE_"

// restoreHookEnv is what a profile's post-restore commands are told of the
// restore, as NAME=value: where the bundle was written and where it is put
// back -- tenant, domain, the cluster's own domain, platform version,
// cluster -- and, for the app, its version that wrote the data and the one
// installed, each store's name in the bundle and here, the databases it made
// itself among them, and its database login's. They are what an app's own data may have cached
// and has to be told of: an address on the old domain, a database's old
// name. A credential is never among them: what the platform made new here
// the command reads from the app's own Secrets, as the app does.
//
// A value the bundle supplied is passed only where it is a plain name,
// domain or version (plainValue); otherwise the variable is empty.
func restoreHookEnv(
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.ComponentProfile,
	tr *TenantReconciler,
) []string {
	source := gentianov1alpha1.RestoreSource{}
	if restore.Status.Source != nil {
		source = *restore.Status.Source
	}
	targetDomain, targetKernelDomain, targetCluster := "", "", ""
	if tr != nil {
		targetDomain, targetKernelDomain, targetCluster = tr.tenantEffectiveDomain(tenant), tr.KernelDomain, tr.ClusterID
	}
	entry := appStatus(&restore.Status.Apps, appName)
	pairs := map[string][]string{}
	sourceTenant := plainValue(restore.Status.SourceTenant)
	var engines []string
	ownedRole := ""
	for _, a := range entry.Artefacts {
		var kind string
		switch a.Kind {
		case bundle.ArtefactPostgresOwned, bundle.ArtefactMariaDBOwned:
			// The databases the app made itself, by the names the restore's
			// Job reported; the PostgreSQL archive names the login they
			// were made by.
			for _, p := range a.Databases {
				from, to, _ := strings.Cut(p, "=")
				pairs["DATABASES"] = append(pairs["DATABASES"], plainValue(from)+"="+plainValue(to))
			}
			if a.Kind == bundle.ArtefactPostgresOwned {
				ownedRole = a.Name
				engines = append(engines, bundle.ArtefactPostgres)
			} else {
				engines = append(engines, bundle.ArtefactMariaDB)
			}
			continue
		case bundle.ArtefactPostgres, bundle.ArtefactMariaDB:
			engines = append(engines, a.Kind)
			kind = "DATABASES"
		case bundle.ArtefactS3:
			kind = "BUCKETS"
		case bundle.ArtefactVolume:
			kind = "VOLUMES"
		default:
			continue
		}
		pairs[kind] = append(pairs[kind], plainValue(a.Name)+"="+plainValue(a.Target))
	}
	pairs["ROLES"] = restoreRolePairs(engines, ownedRole, sourceTenant, tenant.Name, appName)
	vars := [][2]string{
		{"APP", appName},
		{"SOURCE_TENANT", sourceTenant},
		{"TARGET_TENANT", tenant.Name},
		{"SOURCE_DOMAIN", source.Domain},
		{"TARGET_DOMAIN", targetDomain},
		{"SOURCE_KERNEL_DOMAIN", source.KernelDomain},
		{"TARGET_KERNEL_DOMAIN", targetKernelDomain},
		{"SOURCE_PLATFORM_VERSION", source.PlatformVersion},
		{"TARGET_PLATFORM_VERSION", backup.PlatformVersionLine},
		{"SOURCE_CLUSTER", source.Cluster},
		{"TARGET_CLUSTER", targetCluster},
		{"SOURCE_CHART_VERSION", entry.ChartVersion},
		{"TARGET_CHART_VERSION", plainValue(profileChartVersion(profile))},
		{"DATABASES", strings.Join(pairs["DATABASES"], " ")},
		{"ROLES", strings.Join(pairs["ROLES"], " ")},
		{"BUCKETS", strings.Join(pairs["BUCKETS"], " ")},
		{"VOLUMES", strings.Join(pairs["VOLUMES"], " ")},
	}
	env := make([]string, 0, len(vars))
	for _, v := range vars {
		env = append(env, RestoreEnvPrefix+v[0]+"="+v[1])
	}
	return env
}

// restoreRolePairs is the app's database login, "<login in the bundle>=<login
// here>", once for each engine the bundle held a database of for it: the login
// is made new on every restore, whether or not the app made databases of its
// own. The old PostgreSQL role is the one the archive of those databases
// names where there is one, else the platform's name for it in the source
// tenant; the old MariaDB user is always the latter. Where the bundle does
// not record its tenant, an old login it does not name is left out, not
// guessed.
func restoreRolePairs(engines []string, ownedRole, sourceTenant, targetTenant, appName string) []string {
	var roles []string
	for _, kind := range engines {
		from, to := "", ""
		switch kind {
		case bundle.ArtefactPostgres:
			from, to = ownedRole, backup.PostgresRole(targetTenant, appName)
			if from == "" && sourceTenant != "" {
				from = backup.PostgresRole(sourceTenant, appName)
			}
		case bundle.ArtefactMariaDB:
			if sourceTenant != "" {
				from = backup.MariaDBUser(sourceTenant, appName)
			}
			to = backup.MariaDBUser(targetTenant, appName)
		}
		if from == "" {
			continue
		}
		pair := plainValue(from) + "=" + plainValue(to)
		if !slices.Contains(roles, pair) {
			roles = append(roles, pair)
		}
	}
	return roles
}
