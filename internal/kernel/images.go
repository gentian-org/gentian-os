/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	// The MariaDB client is the server's own image (charts/infra/mariadb
	// pins it), to the digest. A floating mariadb:11 resolved to an 11.8
	// client, whose mariadb-dump opens every dump with a statement the 11.1
	// server refuses (SET ... NOTE_VERBOSITY): each dump was taken and none
	// could be loaded again. Bump the two together; a test holds them equal.
	DefaultMariaDBProvisionerImage  = "mariadb:11.1.2-jammy@sha256:2403cc521634162f743b5179ff5b35520daf72df5d9e7e397192af685d9148fd"
	DefaultRedisProvisionerImage    = "redis:7-alpine"
	DefaultMemcachedImage           = "memcached:1.6.38-alpine"
	DefaultKeycloakProvisionerImage = "alpine:3.20"

	// The publishing proxy that stands in a tenant's DMZ (AD-6). nginx
	// because what it does is the one thing nginx is unambiguous about:
	// forward these exact path prefixes and nothing else, to one upstream,
	// with the headers this says and no others.
	//
	// -alpine rather than a distroless build of our own: this pod is on the
	// public internet with no session in front of it, so it wants a stream of
	// upstream security fixes more than it wants a small attack surface we
	// maintain ourselves.
	DefaultPerimeterProxyImage = "nginx:1.27-alpine"
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

// PerimeterProxyImage is the publishing proxy for a tenant's DMZ.
func PerimeterProxyImage() string {
	return envOrDefault("PERIMETER_PROXY_IMAGE", DefaultPerimeterProxyImage)
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
