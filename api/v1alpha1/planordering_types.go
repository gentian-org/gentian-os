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

// PlanOrderingName is the one PlanOrdering a cluster reads.
const PlanOrderingName = "default"

// PlanOrderingSpec says where a tenant orders another resource plan. Only
// the cluster's administrators move a tenant between plans; this is what a
// tenant's own screen shows instead of a choice.
//
// +kubebuilder:validation:XValidation:rule="(has(self.url) ? 1 : 0) + (has(self.email) ? 1 : 0) + (has(self.phone) ? 1 : 0) <= 1",message="at most one of url, email and phone"
type PlanOrderingSpec struct {
	// Text is the notice by language: a two-letter code, such as en or de,
	// and what to show a person reading in it. A reader whose language is
	// not here is shown English, or else the first.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	Text map[string]string `json:"text"`

	// URL is where to order, opened in a new page. {tenant} in it is
	// replaced with the tenant's name.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://\S+$`
	URL string `json:"url,omitempty"`

	// Email is an address to write to.
	// +optional
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	Email string `json:"email,omitempty"`

	// Phone is a number to call, as it is dialed from abroad.
	// +optional
	// +kubebuilder:validation:Pattern=`^\+[0-9][0-9 ]{5,24}$`
	Phone string `json:"phone,omitempty"`
}

// PlanOrdering is the cluster's notice on how to order another plan.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type PlanOrdering struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PlanOrderingSpec `json:"spec"`
}

// PlanOrderingList contains a list of PlanOrdering.
// +kubebuilder:object:root=true
type PlanOrderingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlanOrdering `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlanOrdering{}, &PlanOrderingList{})
}
