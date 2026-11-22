/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"reflect"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A bridge that names spec.bridge.addonsValuesPath is told the connected
// app's add-ons there, as a list, an empty one too; any other component is
// told nothing.
func TestBridgedAddonValues(t *testing.T) {
	bridge := func(path string) *gentianov1alpha1.ComponentProfileSpec {
		return &gentianov1alpha1.ComponentProfileSpec{Bridge: &gentianov1alpha1.BridgeSpec{
			App: &gentianov1alpha1.BridgedApp{Profile: "wiki-base"},
			Ways: []gentianov1alpha1.BridgeWay{{Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
				Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolMCP,
				SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson}},
			AddonsValuesPath: path,
		}}
	}
	cases := []struct {
		name  string
		spec  *gentianov1alpha1.ComponentProfileSpec
		ids   []string
		start map[string]interface{}
		want  map[string]interface{}
	}{
		{
			name: "selected add-ons, at a nested path, beside what is there",
			spec: bridge("tools.active"), ids: []string{"pages", "search"},
			start: map[string]interface{}{"tools": map[string]interface{}{"port": 1}},
			want:  map[string]interface{}{"tools": map[string]interface{}{"port": 1, "active": []interface{}{"pages", "search"}}},
		},
		{
			name: "none selected is an empty list, which says deselected",
			spec: bridge("active"), ids: nil,
			start: map[string]interface{}{"active": []interface{}{"pages"}},
			want:  map[string]interface{}{"active": []interface{}{}},
		},
		{
			name: "a bridge that does not ask is told nothing",
			spec: bridge(""), ids: []string{"pages"},
			start: map[string]interface{}{}, want: map[string]interface{}{},
		},
		{
			name: "an app is told nothing",
			spec: &gentianov1alpha1.ComponentProfileSpec{}, ids: []string{"pages"},
			start: map[string]interface{}{}, want: map[string]interface{}{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &valuesRender{in: &Input{BridgedAddons: tc.ids}, spec: tc.spec}
			r.bridgedAddonValues(tc.start)
			if !reflect.DeepEqual(tc.start, tc.want) {
				t.Fatalf("values = %#v, want %#v", tc.start, tc.want)
			}
		})
	}
}
