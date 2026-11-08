/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	kc "github.com/gentian-org/gentian-os/internal/keycloak"
)

func gentianTenantAppGroup(tenant, profile string) string {
	return kc.TenantAppGroup(tenant, profile)
}

func gentianTenantAppAdminsGroup(tenant string) string {
	return kc.TenantAppAdminsGroup(tenant)
}

func gentianTenantAdminsGroup(tenant string) string {
	return kc.TenantAdminsGroup(tenant)
}

func gentianGroupsJobName(tenantName string) string {
	return kc.GroupsJobName(tenantName)
}
