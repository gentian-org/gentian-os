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

// SecuritySpec declares platform security requests for an AppProfile catalogue entry.
// Cluster administrators approve subsets via PlatformSecurityPolicy; the operator
// intersects requests with the allowlist before compositions apply MAC labels.
type SecuritySpec struct {
	// MacWaivers lists Kyverno MAC policies the app may need when upstream charts
	// cannot satisfy baseline pod security (for example s6-based init as root).
	// +optional
	MacWaivers []MacWaiverRequest `json:"macWaivers,omitempty"`
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
	// Profile is the AppProfile metadata.name that may use this waiver.
	// +kubebuilder:validation:Required
	Profile string `json:"profile"`

	// Policy is the Kyverno ClusterPolicy name.
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`

	// Scope matches MacWaiverRequest.scope on the AppProfile.
	// +kubebuilder:validation:Required
	Scope string `json:"scope"`
}

// MacWaiverLabelKey returns the pod label key for an approved waiver.
func MacWaiverLabelKey(policy string) string {
	return "mac-waiver.gentianos.io/" + policy
}

// MacWaiverApprovedValue is the pod label value stamped when a waiver is approved.
const MacWaiverApprovedValue = "approved"
