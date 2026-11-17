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

// A bridge connects, for a person or for the tenant, across the boundary of
// the browser's session, in either direction. It is a component of its own,
// never part of an app's entry, and a class of its own: nobody opens it, and
// it is reached, or reaches out, only as its ways say. What the platform opens
// for a bridge -- an entry at a door, a host outside, a credential -- is read
// from its ways, and nothing else is opened for it.

package v1alpha1

import (
	"slices"
	"sort"
	"strings"
)

// BridgeCaller is who opens the calls of a way.
// +kubebuilder:validation:Enum=device;outside;agents;app
type BridgeCaller string

const (
	// BridgeCallerDevice is a program on a person's own device: a file sync
	// program, a calendar, an address book.
	BridgeCallerDevice BridgeCaller = "device"
	// BridgeCallerOutside is a service outside the platform.
	BridgeCallerOutside BridgeCaller = "outside"
	// BridgeCallerAgents are the tenant's agents.
	BridgeCallerAgents BridgeCaller = "agents"
	// BridgeCallerApp is the app of the tenant the bridge connects (spec.bridge.app).
	BridgeCallerApp BridgeCaller = "app"
)

// BridgeCallee is who answers the calls of a way.
// +kubebuilder:validation:Enum=app;agents;outside;partner
type BridgeCallee string

const (
	// BridgeCalleeApp is the app of the tenant the bridge connects (spec.bridge.app).
	BridgeCalleeApp BridgeCallee = "app"
	// BridgeCalleeAgents are the tenant's agents, started by the call.
	BridgeCalleeAgents BridgeCallee = "agents"
	// BridgeCalleeOutside is a service outside the platform.
	BridgeCalleeOutside BridgeCallee = "outside"
	// BridgeCalleePartner is a service of one of the platform's partners.
	BridgeCalleePartner BridgeCallee = "partner"
)

// BridgeProtocolName is the protocol the calls of a way are made in.
// +kubebuilder:validation:Enum=mcp;webdav;caldav;carddav
type BridgeProtocolName string

const (
	BridgeProtocolMCP     BridgeProtocolName = "mcp"
	BridgeProtocolWebDAV  BridgeProtocolName = "webdav"
	BridgeProtocolCalDAV  BridgeProtocolName = "caldav"
	BridgeProtocolCardDAV BridgeProtocolName = "carddav"
)

// BridgeSpeaker is whom the calls of a way are made for.
// +kubebuilder:validation:Enum=person;tenant
type BridgeSpeaker string

const (
	// BridgeSpeakerPerson: each call is made as one person, with that
	// person's rights.
	BridgeSpeakerPerson BridgeSpeaker = "person"
	// BridgeSpeakerTenant: calls are made for the tenant as a whole, and
	// only for what every member of the tenant may see.
	BridgeSpeakerTenant BridgeSpeaker = "tenant"
)

// BridgeSpec is what a bridge connects, and the ways its calls go.
// +kubebuilder:validation:XValidation:rule="self.ways.all(w, self.ways.filter(x, x.name == w.name).size() == 1)",message="a way's name is its own within the bridge: it names the way in the tenant's approvals"
// +kubebuilder:validation:XValidation:rule="has(self.app) == self.ways.exists(w, w.caller == 'app' || w.callee == 'app')",message="spec.bridge.app names the app of the tenant a way calls or is called by: present exactly when a way's caller or callee is app"
// +kubebuilder:validation:XValidation:rule="self.ways.filter(w, has(w.root) && w.protocol == 'caldav').size() <= 1 && self.ways.filter(w, has(w.root) && w.protocol == 'carddav').size() <= 1",message="a protocol has one discovery address: at most one way per protocol names a root"
type BridgeSpec struct {
	// App is the app of the tenant the bridge connects: the app's
	// ComponentProfile, which must be installed in the same tenant. A way
	// whose caller or callee is app means this app. A person a call is made
	// for is asked the question a person opening the app is asked.
	// +optional
	App *BridgedApp `json:"app,omitempty"`

	// Ways are the directions the bridge's calls go, each in one protocol
	// and each approved on its own. A bridge in both directions declares
	// two ways.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Ways []BridgeWay `json:"ways"`
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

// BridgeWay is one direction of a bridge's calls: who opens them, who answers
// them, in which protocol, and for whom.
// +kubebuilder:validation:XValidation:rule="self.caller != self.callee",message="a way connects two different sides: its caller and its callee differ"
// +kubebuilder:validation:XValidation:rule="self.caller != 'device' || self.speaksAs == 'person'",message="a program on a person's device speaks as that person"
// +kubebuilder:validation:XValidation:rule="has(self.paths) == (self.callee == 'app' && (self.caller == 'device' || self.caller == 'outside'))",message="paths are where a caller from outside the platform reaches the app: present exactly for a way from a device or an outside service to the app"
// +kubebuilder:validation:XValidation:rule="!has(self.root) || ((self.protocol == 'caldav' || self.protocol == 'carddav') && has(self.paths) && self.paths.exists(p, self.root == p || self.root.startsWith(p.endsWith('/') ? p : p + '/')))",message="root is where a calendar or contact program is sent from the discovery address: only for a way in caldav or carddav, and under one of its paths"
type BridgeWay struct {
	// Name names the way within the bridge, and in the tenant's approvals.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Caller is who opens the calls.
	Caller BridgeCaller `json:"caller"`

	// Callee is who answers them.
	Callee BridgeCallee `json:"callee"`

	// Protocol is what the calls are made in.
	Protocol BridgeProtocolName `json:"protocol"`

	// SpeaksAs is whom the calls are made for.
	SpeaksAs BridgeSpeaker `json:"speaksAs"`

	// Paths are the path prefixes of the app a caller from outside the
	// platform reaches. A caller asks for them as they are written here and
	// the app is asked for the same: answers of these protocols name paths
	// of their own, and a path rewritten on the way in would not match them.
	// Nothing outside these paths is reached through the way, and none of
	// them is the app's root: a way opens what its protocol needs and no
	// more.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^/[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9][A-Za-z0-9._~-]*)*/?$`
	Paths []string `json:"paths,omitempty"`

	// Root is where a calendar or contact program that found the door by
	// its discovery address (RFC 6764: /.well-known/caldav,
	// /.well-known/carddav) is sent, to ask there who the person is and
	// where their calendars or address books are. Under one of Paths, and
	// only for a way in caldav or carddav. The door answers the discovery
	// address with a redirect to it, before anybody is asked who is
	// calling: the redirect names a path and grants nothing.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9][A-Za-z0-9._~-]*)*/?$`
	Root string `json:"root,omitempty"`
}

// BridgeDiscoveryPath is the discovery address of a protocol at the door
// (RFC 6764), or "" for a protocol that has none.
func BridgeDiscoveryPath(protocol BridgeProtocolName) string {
	switch protocol {
	case BridgeProtocolCalDAV, BridgeProtocolCardDAV:
		return "/.well-known/" + string(protocol)
	}
	return ""
}

// discovery is a door way's discovery address, when it names a root.
func (w BridgeWay) discovery() string {
	if w.Root == "" {
		return ""
	}
	return BridgeDiscoveryPath(w.Protocol)
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

const (
	// BridgeDoorLabel is the host label of a tenant's door, where programs
	// on a person's devices reach the tenant's apps: <label>.<the tenant's
	// domain>.
	BridgeDoorLabel = "sync"

	// BridgeEntryName names the one route a bridge is served on at the
	// door. What is approved of it are its ways, each by its own name.
	BridgeEntryName = "bridge"
)

// bridgeRedirectURIs are where a program on a person's own computer is sent
// back to after the person signed in: a loopback address, on any port.
var bridgeRedirectURIs = []string{"http://127.0.0.1/", "http://localhost/"}

// DoorWay reports a way the tenant's door serves: from a program on a
// person's device to the app, in a protocol the app speaks itself over HTTP.
func (w BridgeWay) DoorWay() bool {
	if w.Caller != BridgeCallerDevice || w.Callee != BridgeCalleeApp || len(w.Paths) == 0 {
		return false
	}
	switch w.Protocol {
	case BridgeProtocolWebDAV, BridgeProtocolCalDAV, BridgeProtocolCardDAV:
		return true
	}
	return false
}

// BridgeWay is the profile's way of this name, or nil.
func (p *ComponentProfile) BridgeWay(name string) *BridgeWay {
	if p == nil || p.Spec.Bridge == nil {
		return nil
	}
	for i := range p.Spec.Bridge.Ways {
		if p.Spec.Bridge.Ways[i].Name == name {
			return &p.Spec.Bridge.Ways[i]
		}
	}
	return nil
}

// BridgeDoorWays are the ways of a bridge the tenant's door serves, in the
// profile's order. Nil for a profile that is no bridge, or for one the door
// cannot serve: not of class bridge, or not routed to a Service.
func (p *ComponentProfile) BridgeDoorWays() []BridgeWay {
	if p == nil || p.Spec.Bridge == nil || p.Spec.Bridge.App == nil {
		return nil
	}
	isBridge := false
	for _, c := range p.Spec.Classes {
		isBridge = isBridge || c == ComponentClassBridge
	}
	if api := p.Spec.Package.API; !isBridge || api == nil || api.Backend == nil {
		return nil
	}
	var out []BridgeWay
	for _, w := range p.Spec.Bridge.Ways {
		if w.DoorWay() {
			out = append(out, w)
		}
	}
	return out
}

// BridgePaths are the path prefixes of the named door ways, each once,
// without a trailing slash and in one order; of every door way when no name
// is given.
func (p *ComponentProfile) BridgePaths(ways ...string) []string {
	only := map[string]bool{}
	for _, w := range ways {
		only[w] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range p.BridgeDoorWays() {
		if len(only) > 0 && !only[w.Name] {
			continue
		}
		for _, path := range append(append([]string{}, w.Paths...), w.discovery()) {
			path = strings.TrimSuffix(strings.TrimSpace(path), "/")
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// BridgeRedirects is, for the named ways (every door way when none is
// named), each discovery address the door answers with a redirect, and
// where to.
func (p *ComponentProfile) BridgeRedirects(ways ...string) map[string]string {
	only := map[string]bool{}
	for _, w := range ways {
		only[w] = true
	}
	out := map[string]string{}
	for _, w := range p.BridgeDoorWays() {
		if (len(only) > 0 && !only[w.Name]) || w.discovery() == "" {
			continue
		}
		out[w.discovery()] = w.Root
	}
	return out
}

// BridgePathWays is, for each prefix BridgePaths returns for the same ways,
// the ways that serve it, sorted. A prefix two ways share (a calendar's and
// an address book's principals) names both. The door lets a caller whose
// device password covers some ways reach a prefix only if one of them is
// among the prefix's.
func (p *ComponentProfile) BridgePathWays(ways ...string) map[string][]string {
	only := map[string]bool{}
	for _, w := range ways {
		only[w] = true
	}
	out := map[string][]string{}
	for _, w := range p.BridgeDoorWays() {
		if len(only) > 0 && !only[w.Name] {
			continue
		}
		for _, path := range append(append([]string{}, w.Paths...), w.discovery()) {
			path = strings.TrimSuffix(strings.TrimSpace(path), "/")
			if path == "" || slices.Contains(out[path], w.Name) {
				continue
			}
			out[path] = append(out[path], w.Name)
		}
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

// bridgeEntry is an entry at the tenant's door: taking a token of the
// tenant's realm in place of a session, under paths and no other, to the
// Service the bridge names.
func (p *ComponentProfile) bridgeEntry(name string, paths []string) *ExposureSpec {
	backend := p.Spec.Package.API.Backend
	return &ExposureSpec{
		Name:               name,
		Surface:            SurfaceGateway,
		AuthMode:           AuthModeBearer,
		SubDomain:          BridgeDoorLabel,
		Paths:              paths,
		Backend:            BackendRef{Service: backend.Service, Port: backend.Port},
		ClientRedirectURIs: append([]string(nil), bridgeRedirectURIs...),
	}
}

// BridgeEntry is a bridge as the one route the edge serves at the tenant's
// door, under the paths of every door way. Nil for a profile with no door
// way. What is served of it are the paths of the ways approved
// (BridgeWayEntries); a program signs in once for all of them.
func (p *ComponentProfile) BridgeEntry() *ExposureSpec {
	paths := p.BridgePaths()
	if len(paths) == 0 {
		return nil
	}
	return p.bridgeEntry(BridgeEntryName, paths)
}

// BridgeWayEntries are a bridge's door ways as the entries a perimeter
// approver approves, one per way and named by it: each asks the approval a
// token entry asks for (ExposureKindTokenEntry), for its own paths.
//
// The profile declares no such exposure and may not (a bridge exposes
// nothing of its own): the entries are the platform's reading of
// spec.bridge, so that what is approved, routed and held against a token is
// one thing with what any token entry is.
func (p *ComponentProfile) BridgeWayEntries() []*ExposureSpec {
	var out []*ExposureSpec
	for _, w := range p.BridgeDoorWays() {
		out = append(out, p.bridgeEntry(w.Name, p.BridgePaths(w.Name)))
	}
	return out
}
