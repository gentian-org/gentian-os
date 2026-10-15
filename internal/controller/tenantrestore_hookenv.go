/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
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
// back -- tenant, domain, platform version, cluster -- and, for the app, its
// version that wrote the data and the one installed, and each store's name
// in the bundle and here. They are what an app's own data may have cached
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
	targetDomain, targetCluster := "", ""
	if tr != nil {
		targetDomain, targetCluster = tr.tenantEffectiveDomain(tenant), tr.ClusterID
	}
	entry := appStatus(&restore.Status.Apps, appName)
	pairs := map[string][]string{}
	for _, a := range entry.Artefacts {
		var kind string
		switch a.Kind {
		case bundle.ArtefactPostgres, bundle.ArtefactMariaDB:
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
	vars := [][2]string{
		{"APP", appName},
		{"SOURCE_TENANT", plainValue(restore.Status.SourceTenant)},
		{"TARGET_TENANT", tenant.Name},
		{"SOURCE_DOMAIN", source.Domain},
		{"TARGET_DOMAIN", targetDomain},
		{"SOURCE_PLATFORM_VERSION", source.PlatformVersion},
		{"TARGET_PLATFORM_VERSION", backup.PlatformVersionLine},
		{"SOURCE_CLUSTER", source.Cluster},
		{"TARGET_CLUSTER", targetCluster},
		{"SOURCE_CHART_VERSION", entry.ChartVersion},
		{"TARGET_CHART_VERSION", plainValue(profileChartVersion(profile))},
		{"DATABASES", strings.Join(pairs["DATABASES"], " ")},
		{"BUCKETS", strings.Join(pairs["BUCKETS"], " ")},
		{"VOLUMES", strings.Join(pairs["VOLUMES"], " ")},
	}
	env := make([]string, 0, len(vars))
	for _, v := range vars {
		env = append(env, RestoreEnvPrefix+v[0]+"="+v[1])
	}
	return env
}
