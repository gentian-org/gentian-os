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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantRestoreSpec puts a bundle back into a live tenant.
//
// This is the most destructive resource the platform has. It replaces database
// contents, bucket contents and volume contents with what a bundle recorded,
// and anything written since that bundle was taken is gone. Everything about
// the shape below is arranged around making that hard to do by accident.
type TenantRestoreSpec struct {
	// ExportRef names a TenantExport in this namespace to restore from. It is
	// the ordinary path: the export already knows its bucket, prefix and how it
	// was encrypted.
	// +optional
	ExportRef string `json:"exportRef,omitempty"`

	// Bundle locates a bundle directly, for restoring one this cluster did not
	// produce — a migration, or a rebuild where the original export is gone.
	// +optional
	Bundle *BundleRef `json:"bundle,omitempty"`

	// Apps limits the restore to these profiles.
	//
	// Empty restores every app the bundle's manifest lists that can be
	// restored here, and names the rest, with the reason, in
	// status.notRestored. An app the bundle does not list is never touched.
	//
	// An app named here has to be restorable: if the bundle does not hold it,
	// the tenant does not have it installed, or its build cannot take the
	// data, the whole restore is refused before anything is changed.
	// +optional
	Apps []string `json:"apps,omitempty"`

	// ConfirmTenant must equal the tenant being restored into.
	//
	// A typed confirmation rather than a boolean: `force: true` is something a
	// person sets once and copies forever, while a name has to be looked up and
	// matches only the tenant actually in front of them. It is the difference
	// between confirming and acknowledging.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ConfirmTenant string `json:"confirmTenant"`

	// Decryption supplies the key material for the bundle.
	// +optional
	Decryption *RestoreDecryption `json:"decryption,omitempty"`

	// SkipVersionCheck permits restoring an app's data into a build of the
	// app older than the one that wrote it, or one whose version cannot be
	// compared with it.
	//
	// The bundle's manifest records the chart version each app was running.
	// The same version is restored; a newer installed version is restored
	// too, because an app upgrades older data when it starts. An older one is
	// not, unless this is set: the failure is silent -- an older app reading
	// data written by a newer schema usually starts, serves, and corrupts.
	// Set it only when you know the specific migration is reversible.
	// +optional
	SkipVersionCheck bool `json:"skipVersionCheck,omitempty"`

	// IntoNewTenant says the tenant restored into was made new for this
	// bundle: an import. It changes two things, and nothing else.
	//
	// The data of an app the bundle holds as uninstalled with its data kept
	// is brought with stores made for it, as retained. Without this such
	// data is put back only where the tenant still holds it: an app purged
	// since the bundle was taken stays purged.
	//
	// And the rights the bundle records as granted beyond the defaults are
	// not granted: a right somebody granted in one tenant is not carried
	// into another by a file. The restore names each in its result. What the
	// bundle records as withdrawn is withdrawn here too.
	//
	// The same holds, whatever this says, when the bundle was taken of a
	// tenant of another name or on another cluster.
	// +optional
	IntoNewTenant bool `json:"intoNewTenant,omitempty"`
}

// RestoreDecryption names the key material that opens a bundle.
type RestoreDecryption struct {
	// PassphraseSecretRef holds the passphrase for a passphrase-encrypted
	// bundle, in this tenant's namespace.
	// +optional
	PassphraseSecretRef *SecretKeyRef `json:"passphraseSecretRef,omitempty"`

	// IdentitySecretRef holds an age identity (AGE-SECRET-KEY-1…) for a
	// recipient-encrypted bundle.
	//
	// The platform deliberately does not keep this: the identity matching the
	// cluster's backup recipients is escrowed off-cluster with the recovery
	// kit, so a restore is where an operator demonstrates they still have it.
	// Storing it permanently in the cluster would defeat the escrow.
	// +optional
	IdentitySecretRef *SecretKeyRef `json:"identitySecretRef,omitempty"`
}

// RestoreRights is what a restore does with the entries of the rights store
// a bundle holds. Each entry is written "<who> <relation> <what>", under the
// names of the tenant restored into.
type RestoreRights struct {
	// Grant are written to the store: rights the bundle's tenant had been
	// granted beyond what follows from its entry in git.
	// +optional
	// +listType=atomic
	Grant []string `json:"grant,omitempty"`
	// Withdraw are removed from the store: defaults the bundle's tenant no
	// longer held.
	// +optional
	// +listType=atomic
	Withdraw []string `json:"withdraw,omitempty"`
	// NotBrought are entries the bundle holds that this restore does not
	// write, each with the reason.
	// +optional
	// +listType=atomic
	NotBrought []string `json:"notBrought,omitempty"`
	// Applied says the store holds what Grant and Withdraw say.
	// +optional
	Applied bool `json:"applied,omitempty"`
	// WaitingSince is when the restore began to wait for the operator to
	// attach the tenant in the rights store, which has to come first.
	// +optional
	WaitingSince *metav1.Time `json:"waitingSince,omitempty"`
}

// RestoredArchive is one archived mailbox a restore puts back, or a record
// states was put back.
type RestoredArchive struct {
	// Archive is the archived mailbox's name in the domain's archive.
	// +kubebuilder:validation:MaxLength=128
	Archive string `json:"archive"`
	// Domain is the mail domain whose archive it is in.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Domain string `json:"domain,omitempty"`
	// Address is the address the removed person received at, in the domain
	// of the tenant restored into.
	// +optional
	// +kubebuilder:validation:MaxLength=320
	Address string `json:"address,omitempty"`
	// ArchivedAt is when it was archived, where the bundle was taken; By who
	// chose that, as they were shown there.
	// +optional
	ArchivedAt *metav1.Time `json:"archivedAt,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	By string `json:"by,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	Messages int64 `json:"messages,omitempty"`
}

// TenantRestoreStatus reports progress and what was decided during preflight.
type TenantRestoreStatus struct {
	// Phase reuses the export vocabulary: Pending, Running, Ready, Failed.
	// +optional
	Phase TenantExportPhase `json:"phase,omitempty"`

	// Conditions carry Accepted (admitted, and preflight passed) and Complete.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Apps reports per-app progress.
	// +optional
	Apps []AppExportStatus `json:"apps,omitempty"`

	// Quiesced names apps paused right now, and exists for the same reason as
	// the export's: it is the only record that survives a controller restart,
	// and an app left paused is an outage.
	// +optional
	Quiesced []string `json:"quiesced,omitempty"`

	// Bundle is the bundle actually used, resolved from exportRef or spec.
	// +optional
	Bundle *BundleRef `json:"bundle,omitempty"`

	// BundleSchemaVersion is the format version of the manifest this restore
	// read.
	// +optional
	BundleSchemaVersion int `json:"bundleSchemaVersion,omitempty"`

	// SourceTenant is the tenant the bundle was taken of, as its manifest
	// records it. When it is not the tenant restored into, the bundle's names
	// are another tenant's: databases an app made for itself and the
	// platform's groups are put back under this tenant's names, and the
	// bundle's sign-in clients are not imported.
	// +optional
	SourceTenant string `json:"sourceTenant,omitempty"`

	// NameDerivation says where the restore took the names of what it
	// restores from. "manifest": the bundle's manifest names each artefact
	// and what it was captured from (format 2 and later). "derived": the
	// manifest is of format 1, which names apps and kinds only, so the
	// database, bucket and claim names were derived from the tenant the
	// manifest records and from the claims the tenant has now.
	// +optional
	NameDerivation string `json:"nameDerivation,omitempty"`

	// Complete says whether everything the bundle holds -- or, with
	// spec.apps, everything asked for -- was put back. It is set when the
	// restore ends. False is always explained by notRestored.
	// +optional
	Complete *bool `json:"complete,omitempty"`

	// NotRestored names every app the bundle holds that this restore did not
	// put back, and why. Decided before anything is changed.
	// +optional
	// +listType=atomic
	NotRestored []RestoreOmission `json:"notRestored,omitempty"`

	// Notes are what a restore does not bring back by design, which whoever
	// ran it has to act on.
	// +optional
	// +listType=atomic
	Notes []string `json:"notes,omitempty"`

	// Rights is what this restore does with the entries of the rights store
	// the bundle holds: the ones it grants, the ones it withdraws, and the
	// ones it does not bring. Decided before anything is changed.
	// +optional
	Rights *RestoreRights `json:"rights,omitempty"`

	// ArchivedMailboxes are the archived mailboxes the bundle holds: the
	// mailboxes of people who had been removed from the bundle's tenant with
	// their mail kept. They are put back as archived ones, and each is put
	// on record (MailboxRemoval) so that it is listed and can be deleted.
	// +optional
	// +listType=atomic
	ArchivedMailboxes []RestoredArchive `json:"archivedMailboxes,omitempty"`

	// ImportRemoved records that the uploaded bundle this restore read was
	// removed from the import bucket, which happens once the restore has run
	// to its end. Never set for a restore of a tenant's own backup.
	// +optional
	ImportRemoved bool `json:"importRemoved,omitempty"`

	// PasswordResetRequired flags that members came back without credentials.
	//
	// Keycloak's partial-export carries no password hashes, so a restored realm
	// has accounts nobody can sign in to until they are sent through a reset.
	// Surfaced here because the alternative is an operator discovering it from
	// users who cannot log in.
	// +optional
	PasswordResetRequired bool `json:"passwordResetRequired,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// RestoreOmission is one app a bundle holds that a restore did not put back.
type RestoreOmission struct {
	// App is the app's name in the bundle's manifest.
	App string `json:"app"`
	// Reason says why, in a sentence a person can act on.
	Reason string `json:"reason"`
}

// The values of TenantRestoreStatus.NameDerivation.
const (
	RestoreNamesFromManifest = "manifest"
	RestoreNamesDerived      = "derived"
)

// TenantRestore restores a tenant's data from a bundle.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=trst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Bundle",type=string,JSONPath=`.status.bundle.prefix`
// +kubebuilder:printcolumn:name="Paused",type=string,JSONPath=`.status.quiesced`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TenantRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantRestoreSpec   `json:"spec,omitempty"`
	Status TenantRestoreStatus `json:"status,omitempty"`
}

// TenantRestoreList contains a list of TenantRestore.
// +kubebuilder:object:root=true
type TenantRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantRestore `json:"items"`
}

// IsTerminal reports whether the restore has finished, either way.
func (r *TenantRestore) IsTerminal() bool {
	return r.Status.Phase == TenantExportPhaseReady || r.Status.Phase == TenantExportPhaseFailed
}

func init() {
	SchemeBuilder.Register(&TenantRestore{}, &TenantRestoreList{})
}
