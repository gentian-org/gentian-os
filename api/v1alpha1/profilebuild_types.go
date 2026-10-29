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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ProfileBuild is one build of a profile, as a catalog published it: the
// bundle at one digest, kept on the cluster for as long as a tenant runs it.
//
// A ComponentProfile is one object per name for the whole cluster, and it is
// the newest build any tenant installed. A tenant pinned to another build
// runs it from here: the operator reads its profile out of the bundle this
// object carries, verified against the digest, and the app Composition reads
// the same spec from profileSpec. So two tenants on two builds of one app
// share nothing, and a tenant moved back to a build it ran before finds it
// without fetching anything.
//
// Written by the director beside the profile, into the catalog directory of
// the deployments repository, and applied from there by Argo CD; the
// installer writes the same file for the profile it places. Nothing edits
// it: a build is its digest. The operator reads the bundle, holds it to the
// digest and to what a bundle may hold, and writes the profile's spec it
// found onto the status, which is what the Composition reads.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pb
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.spec.digest`
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.conditions[?(@.type=="Verified")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ProfileBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProfileBuildSpec   `json:"spec"`
	Status ProfileBuildStatus `json:"status,omitempty"`
}

// ProfileBuildSpec is the build.
// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile && self.digest == oldSelf.digest",message="a build is its digest; publish another build rather than changing this one"
type ProfileBuildSpec struct {
	// Profile is the name of the ComponentProfile this is a build of: the
	// app's identity across every build.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Profile string `json:"profile"`

	// Digest is the sha256 of the bundle, "sha256:<hex>", as the catalog's
	// index states it and as an install pins it.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// Origin is which catalog the bundle was fetched from, as the director
	// records it on the profile (cluster/<source> or tenant/<t>/<source>).
	// +kubebuilder:validation:MinLength=1
	Origin string `json:"origin"`

	// Bundle is the published file, base64: the profile and its companions,
	// the bytes that hash to Digest. What the operator verifies and reads.
	// +kubebuilder:validation:MinLength=1
	Bundle string `json:"bundle"`
}

// ProfileBuildStatus is what the operator found in the bundle.
type ProfileBuildStatus struct {
	// ProfileSpec is the bundle's profile spec, with the defaults the API
	// server gives a ComponentProfile, written once the bundle verified.
	// What the app Composition reads for a tenant on this build. Absent
	// until the operator has read the build, and taken off one that does
	// not verify.
	// +optional
	ProfileSpec *ComponentProfileSpec `json:"profileSpec,omitempty"`

	// Conditions: Verified is True when the bundle is the digest it claims
	// and holds what a bundle may; False names what was found.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type ProfileBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProfileBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProfileBuild{}, &ProfileBuildList{})
}
