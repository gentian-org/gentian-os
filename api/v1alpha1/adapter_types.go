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

// An adapter turns one app's abilities into operations that a program acting
// for a person may call. It is a component of its own, never part of the app's
// entry, and a class of its own: it runs in its tenant, nobody opens it, and
// only a component that may speak as a person (requires.services.vouching)
// reaches it.

package v1alpha1

// AdapterSpec is what an adapter is for.
type AdapterSpec struct {
	// Adapts names the app whose interface this adapter uses: the app's
	// ComponentProfile, which must be installed in the same tenant. Installing
	// the adapter opens the way from its pods to that app's pods and nowhere
	// else in the tenant; the app is not asked to cooperate. Absent for an
	// adapter packaged as an API, which reaches a service outside the cluster
	// and no app of the tenant.
	// +optional
	Adapts *AdaptedApp `json:"adapts,omitempty"`
}

// AdaptedApp names the app an adapter fits, and which of its versions.
type AdaptedApp struct {
	// Profile is the ComponentProfile of the app.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Profile string `json:"profile"`

	// Versions are the catalog versions of the app this adapter was
	// reviewed against. Empty means the reviewer named none.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	Versions []string `json:"versions,omitempty"`
}
