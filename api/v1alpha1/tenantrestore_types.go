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

	// Apps limits the restore to these profiles. Empty restores every app the
	// bundle contains that is also installed.
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

	// SkipVersionCheck permits restoring a bundle produced by newer app
	// versions than are installed.
	//
	// Off by default because the failure is silent: an older app reading data
	// written by a newer schema usually starts, serves, and corrupts. Set it
	// only when you know the specific migration is reversible.
	// +optional
	SkipVersionCheck bool `json:"skipVersionCheck,omitempty"`
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
