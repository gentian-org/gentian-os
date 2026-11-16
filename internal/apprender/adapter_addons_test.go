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

// An adapter that names spec.adapter.addonsValuesPath is told the adapted
// app's add-ons there, as a list, an empty one too; any other component is
// told nothing.
func TestAdaptedAddonValues(t *testing.T) {
	adapter := func(path string) *gentianov1alpha1.ComponentProfileSpec {
		return &gentianov1alpha1.ComponentProfileSpec{Adapter: &gentianov1alpha1.AdapterSpec{
			Adapts:           &gentianov1alpha1.AdaptedApp{Profile: "wiki-base"},
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
			spec: adapter("tools.active"), ids: []string{"pages", "search"},
			start: map[string]interface{}{"tools": map[string]interface{}{"port": 1}},
			want:  map[string]interface{}{"tools": map[string]interface{}{"port": 1, "active": []interface{}{"pages", "search"}}},
		},
		{
			name: "none selected is an empty list, which says deselected",
			spec: adapter("active"), ids: nil,
			start: map[string]interface{}{"active": []interface{}{"pages"}},
			want:  map[string]interface{}{"active": []interface{}{}},
		},
		{
			name: "an adapter that does not ask is told nothing",
			spec: adapter(""), ids: []string{"pages"},
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
			r := &valuesRender{in: &Input{AdaptedAddons: tc.ids}, spec: tc.spec}
			r.adaptedAddonValues(tc.start)
			if !reflect.DeepEqual(tc.start, tc.want) {
				t.Fatalf("values = %#v, want %#v", tc.start, tc.want)
			}
		})
	}
}
