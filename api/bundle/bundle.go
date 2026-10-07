/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
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
//
// Version 1 named each app and the kinds of store captured for it, and
// nothing else: not the database, bucket or claim an artefact was taken
// from, nor where in the bundle it is. A restore could therefore only derive
// both from the tenant it was restoring into, and went by what that tenant
// had installed rather than by what the bundle held.
//
// Version 2 says, per app, every artefact: its kind, the name of what it was
// captured from, and its path in the bundle; and the build of the app that
// wrote the data -- its profile, chart version, pinned digest, Helm releases
// and database engine. It adds fields and renames none, so a version 1
// manifest still reads: its apps name no artefact, which is how a reader
// tells (Manifest.NamesArtefacts), and a restore of one falls back to
// deriving the names and says so in its result.
const SchemaVersion = 2

// OldestReadableSchemaVersion is the oldest manifest a restore still reads.
const OldestReadableSchemaVersion = 1

// TenantComponent is the name the tenant-wide captures -- the realm and the
// desktop's database -- are filed under in a manifest's app list. It is not
// an app: a version 1 manifest lists it among them, and a reader leaves it
// out.
const TenantComponent = "gentian-tenant"

// The kinds of artefact a bundle holds.
const (
	// ArtefactPostgres is a custom-format pg_dump of one database.
	ArtefactPostgres = "postgres"
	// ArtefactPostgresOwned is an archive of the databases the app's role
	// owns besides the provisioned one: the ones an app allowed to create
	// its own has made. Inside it, INDEX names them one per line and
	// <line number, from 0>.pgc is each one's custom-format dump.
	ArtefactPostgresOwned = "postgresOwned"
	// ArtefactMariaDB is a gzipped mariadb-dump of one database.
	ArtefactMariaDB = "mariadb"
	// ArtefactMariaDBOwned is an archive of the databases that are the
	// app's besides the provisioned one: the ones named with the
	// provisioned database's name and an underscore as a prefix, which an
	// app allowed to create its own has made. Inside it, INDEX names them
	// one per line and <line number, from 0>.sql.gz is each one's gzipped
	// mariadb-dump.
	//
	// The kind was added within version 2. A reader of version 2 that does
	// not know it refuses the app that has one and says so; it does not
	// restore the app without it.
	ArtefactMariaDBOwned = "mariadbOwned"
	// ArtefactS3 is a tar.gz of one bucket's objects, keys preserved.
	ArtefactS3 = "s3"
	// ArtefactVolume is a tar.gz of one volume claim's contents.
	ArtefactVolume = "volume"
	// ArtefactIdentity is the realm export.
	ArtefactIdentity = "identity"
)

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

	// Digest is the pinned digest of the build that was installed, when the
	// tenant pinned one. Since version 2.
	Digest string `json:"digest,omitempty"`
	// DatabaseEngine is the engine of the app's database, "" without one.
	// Since version 2.
	DatabaseEngine string `json:"databaseEngine,omitempty"`
	// Releases are the Helm releases the app was installed as. Since
	// version 2.
	Releases []string `json:"releases,omitempty"`

	// Stores lists what was captured. In version 1 each entry is a kind and
	// the app's name; since version 2 it is one entry per artefact, with the
	// name of what was captured and the artefact's path.
	Stores []ManifestStore `json:"stores,omitempty"`

	// QuiesceStart and QuiesceEnd bound the window this app's writes were
	// paused, RFC3339. A restore reads them as the instant the app's data is
	// consistent as of.
	QuiesceStart string `json:"quiesceStart,omitempty"`
	QuiesceEnd   string `json:"quiesceEnd,omitempty"`

	// QuiesceMode records how writes were actually paused, which is not always
	// what the profile asked for — see the controller's fallback.
	QuiesceMode string `json:"quiesceMode,omitempty"`

	// BoundSecretKeys is reserved and never written. A bundle carries no
	// stored credential: what the platform seeds is derived again where the
	// bundle is restored, and nothing else from the vault is copied.
	BoundSecretKeys []string `json:"boundSecretKeys,omitempty"`
}

// NamesArtefacts reports whether the manifest says where each artefact is
// and what it was captured from. A version 1 manifest does not.
func (m *Manifest) NamesArtefacts() bool {
	return m != nil && m.SchemaVersion >= 2
}

// ManifestStore is one captured artefact.
type ManifestStore struct {
	// Kind is one of the Artefact kinds: postgres, postgresOwned, mariadb,
	// mariadbOwned, s3, volume or identity.
	Kind string `json:"kind"`
	// Name is the database, bucket or claim captured; for postgresOwned,
	// the role whose databases the archive holds; for mariadbOwned, the
	// provisioned database whose name the archive's databases begin with.
	Name string `json:"name"`
	// Path is the artefact's location within the bundle prefix, without the
	// suffix encryption adds.
	Path string `json:"path"`
	// Release is the Helm release a volume claim recorded when it was
	// captured. Volumes only, and only when the claim recorded one. Since
	// version 2.
	Release string `json:"release,omitempty"`
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
