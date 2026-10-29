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

// An adapter turns one app's abilities into named tools that a program acting
// for a person may call. It is a component of its own, never part of the app's
// entry, and a class of its own: it runs in its tenant, nobody opens it, and
// only a component that may speak as a person (requires.services.vouching)
// reaches it. Whether a tool may be called is decided outside the adapter; the
// tool list below is what that decision is made about, and it is the reviewed
// list in this entry, never what a running adapter says of itself.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// AdapterSpec is what an adapter is for and what it offers.
//
// +kubebuilder:validation:XValidation:rule="self.tools.all(t, !has(t.reverse) || self.tools.exists(u, u.name == t.reverse.name && u.version == t.reverse.version))",message="a tool's reverse names a tool of this adapter, at a version it lists"
type AdapterSpec struct {
	// Adapts names the app whose interface this adapter uses: the app's
	// ComponentProfile, which must be installed in the same tenant. Installing
	// the adapter opens the way from its pods to that app's pods and nowhere
	// else in the tenant; the app is not asked to cooperate. Absent for an
	// adapter packaged as an API, which reaches a service outside the cluster
	// and no app of the tenant.
	// +optional
	Adapts *AdaptedApp `json:"adapts,omitempty"`

	// SpeaksAs is how a call reaches the app as the person on whose behalf it
	// is made, in the order of preference the platform has: a short token for
	// this app from the identity provider, a per-person token of the app's
	// own, or the platform's identity headers. An adapter never acts as
	// itself where a person's data is concerned.
	// +kubebuilder:validation:Enum=token;app-token;headers
	SpeaksAs AdapterSpeaksAs `json:"speaksAs"`

	// Tools are the operations this adapter offers, each at a version. A
	// change to what a tool does, what it needs or its class is a new
	// version, so an adapter update never widens what a person allowed.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	// +listMapKey=version
	Tools []ToolSpec `json:"tools"`
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

// AdapterSpeaksAs is how an adapter's call reaches the app as the person.
// +kubebuilder:validation:Enum=token;app-token;headers
type AdapterSpeaksAs string

const (
	// AdapterSpeaksAsToken uses a short token for this one app, issued by the
	// identity provider for the person. First choice: nothing is stored.
	AdapterSpeaksAsToken AdapterSpeaksAs = "token"
	// AdapterSpeaksAsAppToken uses a per-person token of the app's own,
	// created when a person's permission is confirmed and revoked when the
	// last one ends.
	AdapterSpeaksAsAppToken AdapterSpeaksAs = "app-token"
	// AdapterSpeaksAsHeaders uses the platform's identity headers, for an
	// app that already trusts them. Safe only while nothing but the front
	// door and this adapter reach the app.
	AdapterSpeaksAsHeaders AdapterSpeaksAs = "headers"
)

// ToolSpec is one named operation on the adapted app.
type ToolSpec struct {
	// Name of the tool, for example create-task.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Version of the tool, for example v1.
	// +kubebuilder:validation:Pattern=`^v[0-9]+$`
	// +kubebuilder:validation:MaxLength=8
	Version string `json:"version"`

	// Description is the tool in plain words, by language (en, de, ...). It
	// is the text a person reads when asked to allow it.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	Description map[string]string `json:"description"`

	// Class is whether the tool reads, changes, or changes irreversibly. A
	// tool with no class counts as irreversible, and so does one that
	// reaches outside the platform: what has left cannot be taken back.
	// +optional
	// +kubebuilder:validation:Enum=read;change;irreversible
	Class ToolClass `json:"class,omitempty"`

	// Input is the JSON Schema of the tool's arguments.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Input *runtime.RawExtension `json:"input,omitempty"`

	// Output is the JSON Schema of the tool's answer.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Output *runtime.RawExtension `json:"output,omitempty"`

	// Identifiers are the fields of the answer that the app made up, such as
	// the number of a new task, by their JSON pointer into the answer. A plan
	// replayed against the real app puts the real value in their place.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=256
	Identifiers []string `json:"identifiers,omitempty"`

	// Reverse is the tool of this adapter that undoes this one.
	// +optional
	Reverse *ToolRef `json:"reverse,omitempty"`
}

// ToolClass is what a tool may do.
// +kubebuilder:validation:Enum=read;change;irreversible
type ToolClass string

const (
	// ToolClassRead only reads.
	ToolClassRead ToolClass = "read"
	// ToolClassChange changes something that a later call can take back.
	ToolClassChange ToolClass = "change"
	// ToolClassIrreversible changes something no later call takes back.
	ToolClassIrreversible ToolClass = "irreversible"
)

// ToolRef names one tool of an adapter at one version.
type ToolRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^v[0-9]+$`
	Version string `json:"version"`
}
