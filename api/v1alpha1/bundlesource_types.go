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

// BundleSourceSpec is S3 storage outside the cluster that bundles are read
// from: another cluster's destination, an old cluster's store, a provider's
// bucket a tenant's bundles were left in.
//
// The address is declared here, in git, by the director; the keys are not.
// They are a CredentialRequirement the operator derives from this object and
// the custodian fills, at a vault path derived from the name -- exactly as a
// backup destination's are, and with no field that could name another.
type BundleSourceSpec struct {
	// Endpoint is the S3 API URL, e.g. https://sos-ch-gva-2.exo.io. A scheme
	// is required, as for a destination.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+(/.*)?$`
	Endpoint string `json:"endpoint"`

	// Bucket holds the bundles.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	Bucket string `json:"bucket"`

	// Prefix is where in the bucket the bundles are, each one a prefix of
	// its own directly below it. Empty is the bucket's top.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^$|^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`
	Prefix string `json:"prefix,omitempty"`

	// Region, for a provider that requires one.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Region string `json:"region,omitempty"`

	// AddressingStyle is how a bucket is named in a request to Endpoint:
	// path (the default when empty) or virtualHost.
	// +optional
	AddressingStyle S3AddressingStyle `json:"addressingStyle,omitempty"`

	// Description says what the storage is, for whoever picks a bundle
	// from it.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// BundleSourceStatus reports whether the source can be read.
type BundleSourceStatus struct {
	// CredentialRequirement names the requirement carrying the keys, and
	// CredentialSatisfied reports whether it has been filled.
	// +optional
	CredentialRequirement string `json:"credentialRequirement,omitempty"`
	// +optional
	CredentialSatisfied bool `json:"credentialSatisfied,omitempty"`

	// Conditions carries Ready: whether bundles can be read from here.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// BundleSource is a place outside the cluster bundles are imported from,
// declared by the cluster's administrator.
//
// Cluster-scoped, and the cluster's: what it reaches is read to make a
// tenant, which is the cluster's administrator's to do. A tenant's own
// destination is its BackupPolicy's, and is read for its recovery as it is.
//
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 48 && self.metadata.name.matches('^[a-z0-9]([a-z0-9-]*[a-z0-9])?$')",message="a bundle source's name is a DNS label of at most 48 characters"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bsrc
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.bucket`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.prefix`
// +kubebuilder:printcolumn:name="Credential",type=boolean,JSONPath=`.status.credentialSatisfied`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BundleSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BundleSourceSpec   `json:"spec,omitempty"`
	Status BundleSourceStatus `json:"status,omitempty"`
}

// BundleSourceList contains a list of BundleSource.
// +kubebuilder:object:root=true
type BundleSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BundleSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BundleSource{}, &BundleSourceList{})
}
