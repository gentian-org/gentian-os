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

package kernel

import "os"

// Provisioner images for operator-managed data-plane Jobs. Override via env vars
// on the gentian-os Deployment (see charts/gentian-os/templates/deployment.yaml).
const (
	DefaultPostgresProvisionerImage = "postgres:16-alpine"
	DefaultMariaDBProvisionerImage  = "mariadb:11"
	DefaultRedisProvisionerImage    = "redis:7-alpine"
	DefaultMemcachedImage           = "memcached:1.6.38-alpine"
)

func PostgresProvisionerImage() string {
	return envOrDefault("POSTGRES_PROVISIONER_IMAGE", DefaultPostgresProvisionerImage)
}

func MariaDBProvisionerImage() string {
	return envOrDefault("MARIADB_PROVISIONER_IMAGE", DefaultMariaDBProvisionerImage)
}

func RedisProvisionerImage() string {
	return envOrDefault("REDIS_PROVISIONER_IMAGE", DefaultRedisProvisionerImage)
}

func MemcachedImage() string {
	return envOrDefault("MEMCACHED_IMAGE", DefaultMemcachedImage)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
