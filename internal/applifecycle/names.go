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

func mariadbUserName(tenant, app string) string { return backup.MariaDBUser(tenant, app) }

func s3BucketName(tenant *gentianov1alpha1.Tenant, app string) string {
	return backup.S3Bucket(tenant, app)
}

func redisACLUsername(tenant, app string) string { return backup.RedisACLUser(tenant, app) }

func cnpgDatabaseName(tenant, app string) string { return backup.CNPGDatabaseCR(tenant, app) }

func mariadbDeleteJobName(tenant, app string) string {
	return "mariadb-delete-" + tenant + "-" + app
}

func s3DeleteJobName(tenant, app string) string {
	return "s3-delete-" + tenant + "-" + app
}

func redisACLDeleteJobName(tenant, app string) string {
	return "redis-acl-delete-" + tenant + "-" + app
}
