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

// MailboxChoice is what whoever removes a person decides about that person's
// mailbox. There is no default: the question is asked, and answered.
// +kubebuilder:validation:Enum=archive;delete
type MailboxChoice string

const (
	// MailboxChoiceArchive keeps the mail: the mailbox is moved out of the
	// live mail tree into the archive, where no address opens it.
	MailboxChoiceArchive MailboxChoice = "archive"
	// MailboxChoiceDelete destroys the mailbox with everything in it.
	MailboxChoiceDelete MailboxChoice = "delete"
)

// MailboxRequester is who asked, as the registrar established it.
type MailboxRequester struct {
	// Subject is the person's id at the identity provider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Subject string `json:"subject"`
	// Name is how the person is shown: their address or name at the time.
	// For display only.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Name string `json:"name,omitempty"`
	// RequestID joins this to the registrar's record of the authority and to
	// the identity provider's own event.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	RequestID string `json:"requestID,omitempty"`
	// At is when they asked.
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// MailboxRemovalSpec is the decision.
//
// +kubebuilder:validation:XValidation:rule="self.tenant == oldSelf.tenant && self.address == oldSelf.address && self.mailbox == oldSelf.mailbox",message="the tenant, the address and the choice are what was decided and do not change"
// +kubebuilder:validation:XValidation:rule="has(self.restored) == has(oldSelf.restored)",message="whether the archived mailbox came from a backup does not change"
// +kubebuilder:validation:XValidation:rule="!has(self.restored) || self.mailbox == 'archive'",message="what a backup brings back is an archived mailbox"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.deleteArchive) || has(self.deleteArchive)",message="a request to delete the archived mailbox is not withdrawn"
// +kubebuilder:validation:XValidation:rule="!has(self.deleteArchive) || self.mailbox == 'archive'",message="only an archived mailbox can be deleted later"
type MailboxRemovalSpec struct {
	// Tenant is the tenant the person was removed from.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Tenant string `json:"tenant"`
	// Address is the address the removed person received mail at.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=320
	Address string `json:"address"`
	// Mailbox is the choice: archive or delete.
	Mailbox MailboxChoice `json:"mailbox"`
	// RequestedBy is who removed the person and made the choice.
	RequestedBy MailboxRequester `json:"requestedBy"`
	// Restored says this record was not written at a removal: a restore
	// wrote it, for an archived mailbox it put back from a backup, so that
	// the archive is listed and can be deleted. Nothing is moved or removed
	// for such a record; the operator only reports what it states.
	// +optional
	Restored *RestoredArchive `json:"restored,omitempty"`
	// DeleteArchive, once set, asks for the archived mailbox to be deleted,
	// and says who asked. It is how an archive is destroyed on purpose,
	// later.
	// +optional
	DeleteArchive *MailboxRequester `json:"deleteArchive,omitempty"`
}

// MailboxRemovalPhase is where a removed person's mailbox stands.
// +kubebuilder:validation:Enum=Pending;Archived;Deleted;NoMailbox;Failed
type MailboxRemovalPhase string

const (
	// MailboxRemovalPending: not done yet. The message says what it waits
	// for.
	MailboxRemovalPending MailboxRemovalPhase = "Pending"
	// MailboxRemovalArchived: the mailbox is in the archive.
	MailboxRemovalArchived MailboxRemovalPhase = "Archived"
	// MailboxRemovalDeleted: the mailbox, or the archived mailbox, is gone.
	MailboxRemovalDeleted MailboxRemovalPhase = "Deleted"
	// MailboxRemovalNoMailbox: the address never had a mailbox.
	MailboxRemovalNoMailbox MailboxRemovalPhase = "NoMailbox"
	// MailboxRemovalFailed: it could not be done; the message says why. A
	// failure that may pass is tried again; one that cannot (Refused) is not.
	MailboxRemovalFailed MailboxRemovalPhase = "Failed"
)

// MailboxRemovalStatus is what became of the mailbox.
type MailboxRemovalStatus struct {
	// +optional
	Phase MailboxRemovalPhase `json:"phase,omitempty"`
	// Message says, in a sentence, what happened or what is waited for.
	// +optional
	Message string `json:"message,omitempty"`
	// Refused is set when the operator will not act on this record at all:
	// the address is not one of the tenant's mailboxes, or still belongs to
	// a person. Nothing was changed, and nothing is tried again.
	// +optional
	Refused bool `json:"refused,omitempty"`
	// Domain is the mail domain the mailbox was in.
	// +optional
	Domain string `json:"domain,omitempty"`
	// Archive is the archived mailbox's name in the domain's archive.
	// +optional
	Archive string `json:"archive,omitempty"`
	// ArchivedAt is when the mailbox was archived; DeletedAt when the
	// mailbox, or later its archive, was deleted.
	// +optional
	ArchivedAt *metav1.Time `json:"archivedAt,omitempty"`
	// +optional
	DeletedAt *metav1.Time `json:"deletedAt,omitempty"`
	// SizeBytes and Messages are what the mailbox held when it was archived
	// or deleted.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	Messages int64 `json:"messages,omitempty"`
	// SignInGoneAt is when the operator last saw that no mail password of
	// the address is left with the mail server. The mailbox is touched only
	// some time after that, once the server has read the change.
	// +optional
	SignInGoneAt *metav1.Time `json:"signInGoneAt,omitempty"`
	// Attempts counts the Jobs that failed, and LastFailureAt is when the
	// last one did: the next is started later each time.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	LastFailureAt *metav1.Time `json:"lastFailureAt,omitempty"`
}

// MailboxRemoval records what whoever removed a person decided about that
// person's mailbox, and what became of it.
//
// The registrar writes it, when a person is removed from a tenant that keeps
// mailboxes on the cluster's own mail server; the operator carries it out,
// by a Job beside the mail server's volume, and reports here. For as long as
// the mailbox is archived this is also the record an administrator reads the
// archive from, and where its deletion is asked for.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mbxr
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Choice",type=string,JSONPath=`.spec.mailbox`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MailboxRemoval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MailboxRemovalSpec   `json:"spec,omitempty"`
	Status MailboxRemovalStatus `json:"status,omitempty"`
}

// MailboxRemovalList contains a list of MailboxRemoval.
// +kubebuilder:object:root=true
type MailboxRemovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MailboxRemoval `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MailboxRemoval{}, &MailboxRemovalList{})
}
