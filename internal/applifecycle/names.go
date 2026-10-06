/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// The naming rules themselves live in internal/backup, because provisioning,
// purge and export must agree on them exactly — see that package's doc comment
// for what went wrong when they did not. These remain as local spellings so the
// call sites below read the same as they always did.

func tenantNamespace(tenant string) string { return backup.TenantNamespace(tenant) }

func pgRoleName(tenant, app string) string { return backup.PostgresRole(tenant, app) }

func databaseName(tenant *gentianov1alpha1.Tenant, app string) string {
	return backup.DatabaseName(tenant, app)
}

func cnpgDatabaseName(tenant, app string) string { return backup.CNPGDatabaseCR(tenant, app) }
