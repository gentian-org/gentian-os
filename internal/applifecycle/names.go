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
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenantNamespace(tenant string) string {
	return "tenant-" + tenant
}

func dbRoleName(tenant, app string) string {
	return strings.ReplaceAll(tenant, "-", "_") + "_" + strings.ReplaceAll(app, "-", "_")
}

func databaseName(tenant *gentianov1alpha1.Tenant, app string) string {
	prefix := tenant.Name + "_"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.DatabasePrefix != "" {
		prefix = tenant.Spec.Isolation.DatabasePrefix
	}
	return strings.ReplaceAll(prefix, "-", "_") + strings.ReplaceAll(app, "-", "_")
}

func mariadbUserName(tenant, app string) string {
	return dbRoleName(tenant, app)
}

func s3SafeComponent(value string) string {
	var b strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-':
			b.WriteRune(ch)
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func s3BucketName(tenant *gentianov1alpha1.Tenant, app string) string {
	prefix := tenant.Name + "-"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.S3Prefix != "" {
		prefix = tenant.Spec.Isolation.S3Prefix
	}
	return s3SafeComponent(prefix) + s3SafeComponent(app)
}

func redisACLUsername(tenant, app string) string {
	return tenant + "-" + app
}

func mariadbDeleteJobName(tenant, app string) string {
	return "mariadb-delete-" + tenant + "-" + app
}

func s3DeleteJobName(tenant, app string) string {
	return "s3-delete-" + tenant + "-" + app
}

func redisACLDeleteJobName(tenant, app string) string {
	return "redis-acl-delete-" + tenant + "-" + app
}

func cnpgDatabaseName(tenant, app string) string {
	return "db-" + tenant + "-" + app
}
