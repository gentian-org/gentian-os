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

// TenantExportSpec requests a point-in-time capture of one tenant's data.
//
// The resource lives in the tenant's own namespace, so a tenant's exports stay
// inside its blast radius the way its App claims do. What gets captured is
// derived from the tenant and its profiles, never listed here: an export that
// could be told which stores to read would drift from the stores that exist.
type TenantExportSpec struct {
	// Apps limits the export to these installed profile names. Empty — the
	// normal case — captures every app the tenant has installed, plus the
	// tenant-wide state (identity realm, portal shell database) that belongs to
	// no single app.
	// +optional
	Apps []string `json:"apps,omitempty"`

	// TTLSeconds is how long the bundle should be kept before it may be
	// collected. Zero or unset means keep it until something else removes it;
	// retention policy itself is a Phase 6 concern.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`

	// Encryption selects how the bundle is protected. Omitting it uses the
	// cluster's configured recipients, which is what an unattended export
	// wants; there is no way to ask for no encryption at all.
	// +optional
	Encryption *ExportEncryption `json:"encryption,omitempty"`
}

// ExportEncryptionMode selects who is able to decrypt a bundle.
// +kubebuilder:validation:Enum=recipient;passphrase
type ExportEncryptionMode string

const (
	// ExportEncryptionRecipient encrypts to age public keys. With the cluster's
	// configured recipients this needs nobody present, which is what makes it
	// the mode for scheduled exports and for disaster recovery: the matching
	// identity is escrowed with the recovery kit, so the platform can still
	// read the bundle when the tenant admin who took it is unreachable.
	ExportEncryptionRecipient ExportEncryptionMode = "recipient"
	// ExportEncryptionPassphrase encrypts under a passphrase the requester
	// supplies and the platform does not keep. Nobody but the holder can read
	// the bundle afterwards — including the platform, including support, and
	// including anyone who later compromises the cluster. That is the point,
	// and it is also the risk: a lost passphrase is a lost bundle.
	ExportEncryptionPassphrase ExportEncryptionMode = "passphrase"
)

// ExportEncryption configures bundle protection.
//
// Both modes produce an ordinary age file. A passphrase bundle opens with
// `age -d`, and a recipient bundle with `age -d -i <identity>` — no Gentian
// tooling required, which matters most in exactly the situation a backup is
// for.
type ExportEncryption struct {
	// Mode selects the protection. Defaults to recipient.
	// +optional
	// +kubebuilder:default=recipient
	Mode ExportEncryptionMode `json:"mode,omitempty"`

	// Recipients are age public keys (age1…) to encrypt to, used with
	// mode: recipient. When empty the cluster's configured recipients are
	// used; naming them here instead lets an admin encrypt to a key the
	// platform has no identity for, so the bundle is readable only by them.
	//
	// Every listed recipient can decrypt independently, so this is also how an
	// export is made readable both by its requester and by platform escrow.
	// +optional
	Recipients []string `json:"recipients,omitempty"`

	// PassphraseSecretRef names a Secret in this tenant's namespace holding the
	// passphrase, used with mode: passphrase.
	//
	// A reference rather than a literal: a passphrase written into the spec
	// would be readable by anyone who can get the resource, would sit in
	// etcd, and would be echoed back by every kubectl get. The controller
	// copies the Secret next to the capture Job and removes the copy when the
	// export finishes.
	// +optional
	PassphraseSecretRef *SecretKeyRef `json:"passphraseSecretRef,omitempty"`
}

// SecretKeyRef points at one key in a Secret.
type SecretKeyRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key defaults to "passphrase".
	// +optional
	Key string `json:"key,omitempty"`
}

// Resolved reports the mode to use, applying the default.
func (e *ExportEncryption) Resolved() ExportEncryptionMode {
	if e == nil || e.Mode == "" {
		return ExportEncryptionRecipient
	}
	return e.Mode
}

// PassphraseKey returns the Secret key holding the passphrase.
func (e *ExportEncryption) PassphraseKey() string {
	if e == nil || e.PassphraseSecretRef == nil || e.PassphraseSecretRef.Key == "" {
		return "passphrase"
	}
	return e.PassphraseSecretRef.Key
}

// TenantExportPhase is the overall lifecycle phase of an export.
// +kubebuilder:validation:Enum=Pending;Running;Ready;Failed
type TenantExportPhase string

const (
	// TenantExportPhasePending means the export has been accepted but no app
	// has been captured yet — usually waiting on another export or restore in
	// the same namespace to finish.
	TenantExportPhasePending TenantExportPhase = "Pending"
	// TenantExportPhaseRunning means at least one app is being captured. An
	// app may be paused for writes while the export is in this phase.
	TenantExportPhaseRunning TenantExportPhase = "Running"
	// TenantExportPhaseReady means every requested app was captured and the
	// bundle is complete and readable.
	TenantExportPhaseReady TenantExportPhase = "Ready"
	// TenantExportPhaseFailed means the export gave up. Any app it paused has
	// been resumed; the partial bundle is left in place for diagnosis.
	TenantExportPhaseFailed TenantExportPhase = "Failed"
)

// TenantExportStatus reports progress, and carries the state needed to undo a
// half-finished export after a controller restart.
type TenantExportStatus struct {
	// Phase is the overall lifecycle phase.
	// +optional
	Phase TenantExportPhase `json:"phase,omitempty"`

	// Conditions carry the usual per-concern detail. Accepted reports whether
	// the export was admitted (it is False while another export or restore
	// holds the tenant), and Complete reports the terminal outcome.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Bundle locates the output. It is set as soon as the destination is known,
	// before any data lands, so a failed export can still be found and removed.
	// +optional
	Bundle *BundleRef `json:"bundle,omitempty"`

	// Apps reports per-app progress, including the window each app was paused
	// for. The skew between apps is recorded rather than hidden: an export is
	// consistent within an app, not across them.
	// +optional
	Apps []AppExportStatus `json:"apps,omitempty"`

	// Quiesced names the apps currently paused for writes.
	//
	// This exists so a resume survives a crash. If the operator restarts
	// between pausing an app and capturing it, nothing else records that the
	// app is down, and it would stay down until someone noticed. The controller
	// reads this on every reconcile and resumes anything listed that it is not
	// actively capturing.
	// +optional
	Quiesced []string `json:"quiesced,omitempty"`

	// StartedAt is when the first app began capturing.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the export reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Encryption records how the bundle was protected, so a restore knows what
	// it needs before it starts and an operator can tell at a glance whether a
	// bundle is one the platform can still read.
	// +optional
	Encryption *ExportEncryptionStatus `json:"encryption,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ExportEncryptionStatus reports the protection actually applied.
type ExportEncryptionStatus struct {
	// Mode is the protection used.
	// +optional
	Mode ExportEncryptionMode `json:"mode,omitempty"`

	// Recipients are the public keys the bundle was encrypted to. Public keys
	// only — they are not secret, and recording them is what lets an operator
	// work out which escrowed identity opens a given bundle.
	// +optional
	Recipients []string `json:"recipients,omitempty"`

	// PlatformReadable states plainly whether any platform-held identity can
	// decrypt this bundle. False means the requester's key is the only way in,
	// and no amount of cluster access will substitute for it.
	// +optional
	PlatformReadable bool `json:"platformReadable,omitempty"`
}

// BundleRef locates an export bundle in object storage.
type BundleRef struct {
	// Bucket holds the tenant's bundles. It is provisioned per tenant and is
	// deliberately not one of the app buckets, so exports never capture
	// previous exports.
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Prefix is the directory-like key prefix this bundle occupies. The bundle
	// is a prefix rather than a single object so a capture can stream, a
	// restore can be partial, and no tenant-sized scratch space is needed.
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// AppExportStatus reports the capture of one app.
type AppExportStatus struct {
	// Name is the installed profile name.
	// +optional
	Name string `json:"name,omitempty"`

	// ChartVersion records the chart the app was running when captured.
	// Restoring a bundle into an older app than produced it corrupts silently,
	// so the restore path refuses it — which it can only do if this is here.
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`

	// Stores lists the kinds captured for this app (postgres, mariadb, s3,
	// volumes), derived from the profile's kernelRequirements.
	// +optional
	Stores []string `json:"stores,omitempty"`

	// Phase is this app's own phase, using the same vocabulary as the export.
	// +optional
	Phase TenantExportPhase `json:"phase,omitempty"`

	// QuiesceStart and QuiesceEnd bound the window this app was paused for.
	// Published rather than merely logged: it is the honest measure of what an
	// export costs a tenant, and the evidence that the pause is per app.
	// +optional
	QuiesceStart *metav1.Time `json:"quiesceStart,omitempty"`
	// +optional
	QuiesceEnd *metav1.Time `json:"quiesceEnd,omitempty"`

	// Attempts counts capture Jobs run for this app. Bounded, because the
	// shared Job waiter deletes a failed Job so it gets recreated — right for
	// provisioning, an infinite loop for an export.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// CompletedUnits records each capture Job that finished, by Job name.
	//
	// This is the durable completion record; the Job object is not one. A
	// finished Job can be TTL-collected or swept by the kernel Job GC while a
	// sibling unit is still running, and inferring completion from the Job's
	// existence made the controller re-run a dump it had already uploaded.
	// +optional
	// +listType=atomic
	CompletedUnits []string `json:"completedUnits,omitempty"`

	// Message explains a non-terminal or failed state.
	// +optional
	Message string `json:"message,omitempty"`
}

// TenantExport captures one tenant's data into an encrypted bundle.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=texp;export
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Bundle",type=string,JSONPath=`.status.bundle.prefix`
// +kubebuilder:printcolumn:name="Paused",type=string,JSONPath=`.status.quiesced`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TenantExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantExportSpec   `json:"spec,omitempty"`
	Status TenantExportStatus `json:"status,omitempty"`
}

// TenantExportList contains a list of TenantExport.
// +kubebuilder:object:root=true
type TenantExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantExport `json:"items"`
}

// IsTerminal reports whether the export has finished, either way. Callers use
// it to decide whether a tenant is free for another export or a restore.
func (e *TenantExport) IsTerminal() bool {
	return e.Status.Phase == TenantExportPhaseReady || e.Status.Phase == TenantExportPhaseFailed
}

func init() {
	SchemeBuilder.Register(&TenantExport{}, &TenantExportList{})
}
