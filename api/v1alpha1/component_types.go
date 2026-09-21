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

// Fulfilment is how a tenant's install of a profile is backed.
// +kubebuilder:validation:Enum=auto;dedicated
type Fulfilment string

const (
	// FulfilmentAuto binds to a shared instance where one has been offered to
	// the tenant, and installs a dedicated release otherwise.
	FulfilmentAuto Fulfilment = "auto"
	// FulfilmentDedicated never binds to a backend other tenants use. There is
	// no "shared" value: a tenant cannot demand a backend the platform has not
	// offered.
	FulfilmentDedicated Fulfilment = "dedicated"
)

// ComponentSpec is one installed instance of a ComponentProfile. The profile
// says what may be asked for; this object records what was answered, by whom
// and until when. Enablements and grants are written only by the director,
// from the token of the person who approved.
//
// +kubebuilder:validation:XValidation:rule="self.tenancy == oldSelf.tenancy",message="tenancy is immutable: reinstall to change it"
// +kubebuilder:validation:XValidation:rule="self.profileRef.name == oldSelf.profileRef.name",message="profileRef.name is immutable"
// +kubebuilder:validation:XValidation:rule="self.tenancy == 'tenant' || !has(self.fulfilment) || self.fulfilment == 'auto'",message="fulfilment is a tenant's choice and applies to tenancy tenant only"
// +kubebuilder:validation:XValidation:rule="self.tenancy != 'system' || !has(self.exposures) || self.exposures.size() == 0",message="system components have no exposure"
type ComponentSpec struct {
	// ProfileRef names the catalogue entry this is an instance of.
	ProfileRef ProfileRef `json:"profileRef"`

	// Tenancy is the one mode this instance runs under. It must be a member of
	// the profile's list, and the namespace must be of the matching tier; both
	// are admission checks, because neither is visible from here.
	Tenancy ComponentTenancy `json:"tenancy"`

	// Fulfilment pins how a tenant install is backed.
	// +optional
	// +kubebuilder:default=auto
	Fulfilment Fulfilment `json:"fulfilment,omitempty"`

	// Addons enabled on this instance, by the profile's own names.
	// +optional
	// +listType=set
	Addons []string `json:"addons,omitempty"`

	// Exposures are the perimeter entries of the profile that are switched on.
	// Gateway entries need none: they carry the session and are always on.
	// +optional
	// +listType=map
	// +listMapKey=exposureName
	Exposures []ExposureEnablement `json:"exposures,omitempty"`

	// Privileges are the profile's privilege requests that a person granted.
	// An install with an ungranted request waits; it is neither rejected nor
	// silently run without it.
	// +optional
	// +listType=map
	// +listMapKey=privilege
	Privileges []PrivilegeGrant `json:"privileges,omitempty"`
}

// ProfileRef names a ComponentProfile.
type ProfileRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Digest pins the profile bundle this instance was installed from.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`
}

// ExposureEnablement switches on one perimeter entry of the profile.
//
// authMode is not repeated here: the profile's entry is the one source, and an
// enablement cannot weaken it.
//
// +kubebuilder:validation:XValidation:rule="self.owner == oldSelf.owner",message="owner is immutable: a renewal by someone else is a new enablement"
// +kubebuilder:validation:XValidation:rule="!has(self.reviewAt) || self.reviewAt <= self.expiresAt",message="reviewAt must not be later than expiresAt"
type ExposureEnablement struct {
	// ExposureName names an entry of the profile's expose list whose surface is
	// "perimeter".
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ExposureName string `json:"exposureName"`

	// Host is the public hostname. Empty means the entry's default host in the
	// tenant's zone. A vanity host is admitted only if the tenant's approved
	// domains include it.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	Host string `json:"host,omitempty"`

	// Owner is the Keycloak subject that enabled the surface, set by the
	// director from the caller's token.
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// ExpiresAt bounds the exposure, and is always set: a public surface with
	// no end is not something anybody decided. At expiry the operator treats
	// the enablement as absent; the entry stays in git as history.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// ReviewAt is when the owner and the perimeter approver are asked to renew
	// or revoke.
	// +optional
	ReviewAt *metav1.Time `json:"reviewAt,omitempty"`
}

// PrivilegeGrant answers one entry of the profile's privilege request.
//
// +kubebuilder:validation:XValidation:rule="self.approver == oldSelf.approver && self.approvedAt == oldSelf.approvedAt",message="approver and approvedAt are immutable: a new approval is a new grant"
type PrivilegeGrant struct {
	// Privilege names one entry of the profile's request, as <kind>/<name>,
	// where kind is podSecurity, egress or clusterRoles.
	// +kubebuilder:validation:Pattern=`^(podSecurity|egress|clusterRoles)/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Privilege string `json:"privilege"`

	// Approver is the Keycloak subject who said yes, set by the director from
	// the caller's token.
	// +kubebuilder:validation:MinLength=1
	Approver string `json:"approver"`

	ApprovedAt metav1.Time `json:"approvedAt"`

	// Reason in the approver's words, not the profile's.
	// +kubebuilder:validation:MinLength=10
	Reason string `json:"reason"`

	// ExpiresAt bounds the grant. A waiver with no expiry is a waiver nobody
	// reviews.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// ComponentStatus is the observed state of a Component.
type ComponentStatus struct {
	// Fulfilment is "dedicated" or "shared". Availability decided it at install
	// and a later offer does not move a running instance.
	// +optional
	// +kubebuilder:validation:Enum=dedicated;shared
	Fulfilment string `json:"fulfilment,omitempty"`

	// SharedInstance names the backend when Fulfilment is "shared".
	// +optional
	SharedInstance string `json:"sharedInstance,omitempty"`

	// PendingPrivileges are requests of the profile that nobody has granted
	// yet, as <kind>/<name>. While any is listed the install waits.
	// +optional
	// +listType=set
	PendingPrivileges []string `json:"pendingPrivileges,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Component is an installed instance of a ComponentProfile.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=comp
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profileRef.name`
// +kubebuilder:printcolumn:name="Tenancy",type=string,JSONPath=`.spec.tenancy`
// +kubebuilder:printcolumn:name="Fulfilment",type=string,JSONPath=`.status.fulfilment`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type Component struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComponentSpec   `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

// ComponentList contains a list of Component.
//
// +kubebuilder:object:root=true
type ComponentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Component `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Component{}, &ComponentList{})
}
