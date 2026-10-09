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
	// DefaultSignInSidecarImage is the platform's sign-in sidecar: the
	// program that signs people in to an app that can do neither OIDC nor
	// SAML (internal/controller/signin_sidecar.go).
	//
	// One build, named twice: by a tag no later build takes, and by the
	// digest of what that tag held. This is the program that decides whether
	// a sign-in is genuine, so the build that was tested is the build that
	// runs, and neither a registry nor a rebuild can change it.
	//
	// It is built by gentian-apps, from images/gentian-sidecar-sso-saml. A
	// build of that repository's develop branch is develop-<commit>; a
	// release is its version.
	DefaultSignInSidecarImage = "ghcr.io/gentian-org/sidecar-sso-saml:develop-9cdb51a@sha256:623d425da55c5243df706fb3863ea02b6a91692c080f002c4344e3b6f910d245"
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

// SignInSidecarImage is the sign-in sidecar the operator runs beside an app
// whose profile declares one.
func SignInSidecarImage() string {
	return envOrDefault("SIGN_IN_SIDECAR_IMAGE", DefaultSignInSidecarImage)
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
