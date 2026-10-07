/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/licencereport"
)

// Options configures the lifecycle service.
type Options struct {
	OpenBaoNamespace  string
	OperatorNamespace string
	OperatorSA        string
	// MetricsEnabled turns on the live-consumption series read from
	// metrics.k8s.io. Off in a cluster without metrics-server, where the
	// resources API still reports the ceiling and what is committed under it.
	MetricsEnabled bool
	// LicenceReport is whether this cluster reports what it runs, and to
	// where: what the read of the last report answers from.
	LicenceReport licencereport.Settings
	// Vault is where apps' credentials are stored: what a purge deletes them
	// from and the retained-data read lists. Nil when the operator has no
	// vault configured; a purge then fails rather than claim the credentials
	// destroyed.
	Vault CredentialStore
	// LiveReader reads the API server itself rather than the manager's
	// cache. The catalogue's residue is listed and removed on what it
	// answers, because what is deleted there is decided on the answer. Nil
	// reads through the service's client.
	LiveReader client.Reader
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
