/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/usagestatistics"
)

// Options configures the lifecycle service.
type Options struct {
	OperatorNamespace string
	OperatorSA        string
	// MetricsEnabled turns on the live-consumption series read from
	// metrics.k8s.io. Off in a cluster without metrics-server, where the
	// resources API still reports the ceiling and what is committed under it.
	MetricsEnabled bool
	// UsageStatistics is whether this cluster reports what it runs, and to
	// where: what the read of the last report answers from.
	UsageStatistics usagestatistics.Settings
	// Vault is where apps' credentials are stored: what a purge deletes them
	// from and the retained-data read lists. Nil when the operator has no
	// vault configured; a purge then fails rather than claim the credentials
	// destroyed.
	Vault CredentialStore
	// Authorization is the arbiter's platform path: what a purge deletes an
	// app's authorization store through. Nil leaves the store, and the
	// purge says so rather than claim it destroyed.
	Authorization AuthorizationStores
	// LiveReader reads the API server itself rather than the manager's
	// cache. The catalogue's residue is listed and removed on what it
	// answers, because what is deleted there is decided on the answer. Nil
	// reads through the service's client.
	LiveReader client.Reader
	// TenancyMode is whether the cluster carries one user tenant or many, as
	// the operator runs: what decides whether a tenant's administrator may
	// remove what an app's bundle left behind. Empty is multi.
	TenancyMode string
	// DeploymentsRepo is the URL of the repository the cluster is declared
	// in. The operator holds no checkout of it and no credential for it; it
	// is how the operator tells which of Argo CD's objects read that
	// repository, when the director says it has pushed. Empty refreshes
	// nothing.
	DeploymentsRepo string
	// RefreshEvery is the least time between two such refreshes; zero is
	// refreshEvery. Tests set it.
	RefreshEvery time.Duration
}

// Result is returned from lifecycle operations.
type Result struct {
	Status   string   `json:"status"`
	Tenant   string   `json:"tenant"`
	Profile  string   `json:"profile"`
	Purged   bool     `json:"purged,omitempty"`
	Ready    bool     `json:"ready,omitempty"`
	Message  string   `json:"message,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// What a purge did: Destroyed are the kinds of data that are now gone,
	// in the order they went, and Complete is true. A purge that could not
	// destroy everything answers with an error, not with this.
	Complete  *bool    `json:"complete,omitempty"`
	Destroyed []string `json:"destroyed,omitempty"`
}
