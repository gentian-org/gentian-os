/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package provisioner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared stores are composed by charts, and the charts write the address
// an app is handed. An app's network policy opens the port named here, so a
// chart that moved its server to another port would leave every app with the
// right address and no way to it. Each chart is held to the number.
func TestTheStoreChartsAnswerOnThePortsAppsAreGiven(t *testing.T) {
	services := filepath.Join("..", "..", "..", "kernel", "services")
	infra := filepath.Join("..", "..", "..", "charts", "infra")
	for _, c := range []struct{ what, file, want string }{
		{"the MariaDB address", filepath.Join(services, "infra-mariadb", "manifests", "templates", "externalsecret.yaml"),
			fmt.Sprintf(`port: "%d"`, MariaDBPort)},
		{"the MariaDB pod's port", filepath.Join(infra, "mariadb", "values.yaml"),
			fmt.Sprintf("containerPort: %d", MariaDBPort)},
		{"the Redis address", filepath.Join(services, "infra-redis", "manifests", "templates", "externalsecret.yaml"),
			fmt.Sprintf(`port: "%d"`, RedisPort)},
		{"the Redis pod's port", filepath.Join(infra, "redis", "values.yaml"),
			fmt.Sprintf("redis: %d", RedisPort)},
		{"the MinIO address", filepath.Join(services, "infra-minio", "manifests", "templates", "externalsecret.yaml"),
			fmt.Sprintf(".svc.cluster.local:%d", ObjectStoragePort)},
		{"the MinIO pod's port", filepath.Join(infra, "minio", "values.yaml"),
			fmt.Sprintf("api: %d", ObjectStoragePort)},
		// And the port each store's NetworkPolicy admits its clients on
		// (templates/networkpolicy.yaml beside each of these): the server's
		// side of the rule the tenant's side opens with the same constant.
		{"the port PostgreSQL's policy admits", filepath.Join("..", "..", "..", "kernel", "data", "tenant-postgres", "values.yaml"),
			fmt.Sprintf("\n    postgresql: %d\n", PostgresPort)},
		{"the port MariaDB's policy admits", filepath.Join(services, "infra-mariadb", "manifests", "values.yaml"),
			fmt.Sprintf("\n    mariadb: %d\n", MariaDBPort)},
		{"the port Redis's policy admits", filepath.Join(services, "infra-redis", "manifests", "values.yaml"),
			fmt.Sprintf("\n    redis: %d\n", RedisPort)},
		{"the port MinIO's policy admits", filepath.Join(services, "infra-minio", "manifests", "values.yaml"),
			fmt.Sprintf("\n    api: %d\n", ObjectStoragePort)},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %s does not say %q", c.what, c.file, c.want)
		}
	}
}
