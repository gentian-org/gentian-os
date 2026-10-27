/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

// PlatformMarkerAttribute, with the value PlatformMarkerValue, marks a client
// or a group in a realm as one the platform declares: a Composition's
// provider-keycloak resource, or a Job of the operator's, makes it and keeps
// it, under the id it has in the realm. A client carries it as an attribute
// (the resource's extraConfig), a group as a one-valued attribute.
//
// A realm import never makes such a client or group from a bundle: the
// platform makes it on the cluster it is restored onto, and a copy made from
// the bundle first would stand where the platform's resource expects to make
// its own, which it then cannot (internal/backup RealmImportJob).
const (
	PlatformMarkerAttribute = "gentianos.io/managed-by"
	PlatformMarkerValue     = "platform"
)

// PlatformClientPatterns match the clientIds the platform gives the clients it
// makes under fixed names, for a bundle taken before the marker was set.
// Each names where the clients come from:
//
//   - gentian-edge-<zone>, gentian-edge-exchange: tenant-default's edge clients;
//   - vouch-<provider>: tenant-default's vouching clients;
//   - broker-<tenant> (the identity Job, in the kernel realm) and
//     broker-to-<name> (tenant-default);
//   - gentian-dovecot: tenant-default, cluster-default and the Dovecot OIDC Job;
//   - openbao: cluster-default's client, under its default name;
//   - https://<host>/sso: app-default's sign-in sidecar client;
//   - gentian-registrar-admin, gentian-director-admin (retired): the
//     operator's administrative clients;
//   - gentian-argocd, headlamp, grafana, gentian-cli, litellm-dashboard: the
//     installer's clients in the kernel realm.
//
// An app's own client is named by its profile (requires.services.identity.oidc
// .clientId, or .saml.entityId), which no pattern can know: the restore is
// given those names from the catalogue (backup.RealmSource.PlatformClients).
var PlatformClientPatterns = []string{
	`^gentian-edge-`,
	`^vouch-`,
	`^broker-`,
	`^gentian-dovecot$`,
	`^openbao$`,
	`^https://[^/]+/sso$`,
	`^gentian-(registrar|director)-admin$`,
	`^(gentian-argocd|headlamp|grafana|gentian-cli|litellm-dashboard)$`,
}

// PlatformGroupPattern matches the names of the groups the platform makes,
// for a bundle taken before the marker was set: the tenant's
// gentian:tenant:<tenant>:… (members, admins, app-admins and
// app:<profile>; TenantMembersGroup and the others) and the kernel realm's
// gentian:platform:… (the platform administrators' group).
const PlatformGroupPattern = `^gentian:(tenant|platform):`

// PlatformMarkerShellJSON is the marker as a client's attributes, written for
// a JSON body inside a double-quoted shell string: what a Job that makes a
// client puts into the body it posts.
const PlatformMarkerShellJSON = `\"attributes\":{\"` + PlatformMarkerAttribute + `\":\"` + PlatformMarkerValue + `\"}`
