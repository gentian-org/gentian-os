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


// An action is a step another component runs: one image at one build, started
// for one call and gone when the call ends. It is a component of its own and a
// class of its own, so that a tenant's administrator installs, pins and
// removes a step as they do an app. Installing one deploys nothing. A run of
// it is started through the dispatcher, in the tenant's namespace for runs,
// and reaches the component that asked for it and nothing else.

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ActionRuntime is what runs an action.
// +kubebuilder:validation:Enum=container
type ActionRuntime string

const (
	// ActionRuntimeContainer runs the action's image as a pod per run.
	ActionRuntimeContainer ActionRuntime = "container"
)

// ActionSpec is what a caller needs to know to run an action, and what an
// administrator sees before installing it. What an action takes and returns
// is said here and not by its image: the image is one runtime's way of
// keeping the contract.
type ActionSpec struct {
	// Runtime is what runs the action.
	// +optional
	// +kubebuilder:default=container
	Runtime ActionRuntime `json:"runtime,omitempty"`

	// Command is what a run starts in the action's image. It is given the
	// run's inputs on standard input and writes its outputs to standard
	// output; the environment names the address and the credential at which
	// the caller answers for anything more. It is stated because a run wraps
	// it, and an image's own entrypoint cannot be wrapped unseen.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=1024
	// +listType=atomic
	Command []string `json:"command"`

	// Inputs is the JSON Schema of what a run takes.
	// +optional
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`

	// Outputs is the JSON Schema of what a run returns.
	// +optional
	Outputs *apiextensionsv1.JSON `json:"outputs,omitempty"`

	// Adapters names the adapters whose tools the calling component may call
	// on a run's behalf, by their ComponentProfile. A run reaches no adapter
	// itself. It is a statement for the administrator who installs the
	// action; the caller enforces it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Adapters []string `json:"adapters,omitempty"`

	// DeadlineSeconds is the longest one attempt of a run may take. A caller
	// may ask for less and never for more.
	// +optional
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	DeadlineSeconds int32 `json:"deadlineSeconds,omitempty"`

	// Resources is what one attempt of a run is given.
	// +optional
	Resources *ActionResources `json:"resources,omitempty"`
}

// ActionResources is the CPU and memory of one attempt. Each is both what is
// reserved and the most that is used: a step that may take more than it
// reserves makes the tenant's quota for runs a guess.
type ActionResources struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}
