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

// TenantDomainSpec binds a tenant to a custom domain.
type TenantDomainSpec struct {
	// Domain is where the tenant is served instead of <tenant>.<kernelDomain>:
	// its zone, its hosts, its mail and its people's logins all move to it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	// +kubebuilder:validation:MaxLength=253
	Domain string `json:"domain"`
}

// TenantDomain is an extension point: the tenant it is named after is served
// at spec.domain. The OS reads it and has no way to write one -- no field on
// the Tenant, no director route, no console form -- so a cluster has custom
// domains exactly when an extension that writes TenantDomains is installed.
// The operator copies the domain to the Tenant's status.domain, which is what
// every consumer reads.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=tdomain
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TenantDomain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TenantDomainSpec `json:"spec,omitempty"`
}

// TenantDomainList contains a list of TenantDomain.
// +kubebuilder:object:root=true
type TenantDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantDomain `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TenantDomain{}, &TenantDomainList{})
}
