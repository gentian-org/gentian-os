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

// A bridge connects one app to callers that act for a person and cannot hold
// the browser's session. It is a component of its own, never part of the
// app's entry, and a class of its own: nobody opens it, it exposes nothing,
// and its callers reach it through a door of the platform's that checks every
// one of them.

package v1alpha1

// BridgeKind is who is on the far side of a bridge.
// +kubebuilder:validation:Enum=external
type BridgeKind string

const (
	// BridgeKindExternal is a bridge to programs outside the cluster that
	// speak a standard protocol for a person: a file sync program, a
	// calendar, an address book.
	BridgeKindExternal BridgeKind = "external"
)

// BridgeProtocolName is a standard protocol an external bridge offers.
// +kubebuilder:validation:Enum=webdav;caldav;carddav
type BridgeProtocolName string

const (
	BridgeProtocolWebDAV  BridgeProtocolName = "webdav"
	BridgeProtocolCalDAV  BridgeProtocolName = "caldav"
	BridgeProtocolCardDAV BridgeProtocolName = "carddav"
)

// BridgeSpec is what a bridge connects.
type BridgeSpec struct {
	// Kind is who is on the far side.
	Kind BridgeKind `json:"kind"`

	// App names the app on the near side: the app's ComponentProfile, which
	// must be installed in the same tenant. A caller of the bridge is asked
	// the question a person opening that app is asked.
	App BridgedApp `json:"app"`

	// Protocols are what the bridge offers of the app, each under the paths
	// it answers at. A protocol is named once.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Protocols []BridgeProtocol `json:"protocols"`
}

// BridgedApp names the app a bridge connects, and which of its versions.
type BridgedApp struct {
	// Profile is the ComponentProfile of the app.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Profile string `json:"profile"`

	// Versions are the catalog versions of the app this bridge was reviewed
	// against. Empty means the reviewer named none.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=64
	Versions []string `json:"versions,omitempty"`
}

// BridgeProtocol is one protocol a bridge offers, and where.
type BridgeProtocol struct {
	// Name is the protocol.
	Name BridgeProtocolName `json:"name"`

	// Paths are the path prefixes the protocol is answered under. A caller
	// asks for them as they are written here and the backend is asked for the
	// same: answers of these protocols name paths of their own, and a path
	// rewritten on the way in would not match them. Nothing outside these
	// paths is reached through the bridge, and none of them is the app's
	// root: a bridge opens what a protocol needs and no more.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^/[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9][A-Za-z0-9._~-]*)*/?$`
	Paths []string `json:"paths"`
}

// APIBackend is a Service inside the tenant that an entry packaged as an API
// is answered by.
type APIBackend struct {
	// Service is the Service's name in the tenant's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Service string `json:"service"`

	// Port is the Service's port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}
