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

// Package bundle is the format of a tenant bundle's index: the manifest that
// says what a bundle holds, and the unencrypted header that says how to open
// it. It is the part of an export that another implementation has to read and
// write the same way, which is why it lives with the API and not with the
// code that happens to produce it.
package bundle

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// SchemaVersion is bumped whenever the manifest's shape changes in a
// way a reader must notice. A restore refuses a version it does not know
// rather than guessing at fields that moved.
const SchemaVersion = 1

// Manifest is the index of a bundle, and the only part of it a restore reads
// before deciding whether it can proceed.
//
// It exists because a bundle outlives the cluster that produced it. Two years
// on, the question "what was running when this was taken, and can this platform
// still read it" has to be answerable from the bundle alone — not from a wiki,
// and not by unpacking a hundred gigabytes of dumps to find out.
type Manifest struct {
	SchemaVersion int `json:"schemaVersion"`

	// Tenant and TenantSpec snapshot the claim. A restore into a fresh cluster
	// re-applies the spec and lets the platform re-compose the tenant, so the
	// bundle never needs to carry provisioned infrastructure.
	Tenant     string                       `json:"tenant"`
	TenantSpec *gentianov1alpha1.TenantSpec `json:"tenantSpec,omitempty"`

	// Export names the TenantExport that produced this bundle.
	Export string `json:"export"`

	// CreatedAt is when the export started, RFC3339.
	CreatedAt string `json:"createdAt"`

	// OperatorVersion records the gentian-os build that wrote the bundle.
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// Apps records what was captured per app, including the pause window. The
	// windows are published rather than smoothed over: an export is consistent
	// within an app and not across them, and the timestamps are what make that
	// visible instead of merely documented.
	Apps []ManifestApp `json:"apps"`

	// Identity records the realm capture, when one was taken.
	Identity *ManifestIdentity `json:"identity,omitempty"`

	// Shell records the portal shell database, which belongs to the tenant
	// rather than to any app.
	Shell *ManifestStore `json:"shell,omitempty"`
}

// ManifestApp is one app's entry in the bundle index.
type ManifestApp struct {
	Name         string `json:"name"`
	Profile      string `json:"profile,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`

	// Stores lists what was captured, by kind.
	Stores []ManifestStore `json:"stores,omitempty"`

	// QuiesceStart and QuiesceEnd bound the window this app's writes were
	// paused, RFC3339. A restore reads them as the instant the app's data is
	// consistent as of.
	QuiesceStart string `json:"quiesceStart,omitempty"`
	QuiesceEnd   string `json:"quiesceEnd,omitempty"`

	// QuiesceMode records how writes were actually paused, which is not always
	// what the profile asked for — see the controller's fallback.
	QuiesceMode string `json:"quiesceMode,omitempty"`

	// BoundSecretKeys names the secrets carried for this app. Names only: the
	// values are in the bundle, and repeating them in an index that tooling
	// prints would defeat the point of encrypting it.
	BoundSecretKeys []string `json:"boundSecretKeys,omitempty"`
}

// ManifestStore is one captured artefact.
type ManifestStore struct {
	// Kind is postgres, mariadb, s3, volume or identity.
	Kind string `json:"kind"`
	// Name is the database, bucket or claim captured.
	Name string `json:"name"`
	// Path is the artefact's location within the bundle prefix.
	Path string `json:"path"`
}

// ManifestIdentity records the realm capture.
type ManifestIdentity struct {
	Realm string `json:"realm"`
	Path  string `json:"path"`
	// PasswordsIncluded is always false today, and is recorded rather than
	// assumed: a restore has to tell members their password will not come back,
	// and it should read that from the bundle rather than from a constant that
	// might change.
	PasswordsIncluded bool `json:"passwordsIncluded"`
}

// Info is the one file in a bundle that is not encrypted.
//
// It says what the bundle is and how to open it, and nothing about what is
// inside. Without it a recipient facing a directory of .age files has to guess
// which identity applies; with it, the manifest — which carries the tenant's
// spec and app inventory — can stay encrypted like everything else.
type Info struct {
	SchemaVersion int      `json:"schemaVersion"`
	Tenant        string   `json:"tenant"`
	Export        string   `json:"export"`
	CreatedAt     string   `json:"createdAt"`
	Encryption    string   `json:"encryption"`
	Recipients    []string `json:"recipients,omitempty"`
	// HowToDecrypt is a literal command, because the person reading this is
	// having a bad day and should not have to look anything up.
	HowToDecrypt string `json:"howToDecrypt"`
}
