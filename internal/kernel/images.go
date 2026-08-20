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
	// The postgres client major must be >= the CNPG server major: psql
	// tolerates skew in either direction, but pg_dump refuses a server newer
	// than itself — provisioning kept working against a 17 server while every
	// backup died instantly on "server version mismatch". The server version
	// is not pinned in this repo (the CNPG operator's default applies, 17.x
	// today), so bump this alongside CNPG operator upgrades.
	DefaultPostgresProvisionerImage = "postgres:17-alpine"
	DefaultMariaDBProvisionerImage  = "mariadb:11"
	DefaultRedisProvisionerImage    = "redis:7-alpine"
	DefaultMemcachedImage           = "memcached:1.6.38-alpine"
	DefaultKeycloakProvisionerImage = "alpine:3.20"
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

func KeycloakProvisionerImage() string {
	return envOrDefault("KEYCLOAK_PROVISIONER_IMAGE", DefaultKeycloakProvisionerImage)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
