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
	networkingv1 "k8s.io/api/networking/v1"
)

// SecuritySpec declares platform security requests for a catalogue entry.
// Cluster administrators approve subsets via PlatformSecurityPolicy; the operator
// intersects requests with the allowlist before compositions apply MAC labels.
type SecuritySpec struct {
	// MacWaivers lists Kyverno MAC policies the app may need when upstream charts
	// cannot satisfy baseline pod security (for example s6-based init as root).
	// +optional
	MacWaivers []MacWaiverRequest `json:"macWaivers,omitempty"`

	// Egress lists specific outbound network rules required by the application.
	// +optional
	Egress []networkingv1.NetworkPolicyEgressRule `json:"egress,omitempty"`
}

// MacWaiverRequest identifies a MAC policy exception scope requested by the profile.
type MacWaiverRequest struct {
	// Policy is the Kyverno ClusterPolicy name (for example gentian-require-non-root).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Policy string `json:"policy"`

	// Scope narrows the waiver to a composition component (for example sidecar-jitsi).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Scope string `json:"scope"`
}

// AllowedMacWaiver is a cluster-admin approved MAC waiver for a catalogue profile.
type AllowedMacWaiver struct {
	// Profile is the ComponentProfile metadata.name that may use this waiver.
	// +kubebuilder:validation:Required
	Profile string `json:"profile"`

	// Policy is the Kyverno ClusterPolicy name.
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`

	// Scope matches MacWaiverRequest.scope on the profile.
	// +kubebuilder:validation:Required
	Scope string `json:"scope"`
}

// AllowedClusterRole permits one of the platform's cluster roles for one
// catalogue profile on this cluster.
type AllowedClusterRole struct {
	// Profile is the ComponentProfile metadata.name that may be bound to it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Profile string `json:"profile"`

	// Role is the platform's name of the role, as a profile asks for it in
	// requires.privileges.clusterRoles[].name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Role string `json:"role"`
}

// MacWaiverLabelKey returns the pod label key for an approved waiver.
func MacWaiverLabelKey(policy string) string {
	return "mac-waiver.gentianos.io/" + policy
}

// MacWaiverApprovedValue is the pod label value stamped when a waiver is approved.
const MacWaiverApprovedValue = "approved"
