/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	"encoding/json"
	"fmt"
)

// GroupAttributesAnnotation is the ComponentProfile annotation that declares
// the attributes of the profile's app group, as a JSON object.
const GroupAttributesAnnotation = "gentianos.io/keycloak-group-attributes"

// ParseGroupAttributes reads a GroupAttributesAnnotation value as Keycloak's
// attribute lists: a list item by item, a string as itself, and any other
// value, null included, as its fmt.Sprint form. The caller decides what a
// malformed value means.
func ParseGroupAttributes(raw string) (map[string][]string, error) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	attrs := make(map[string][]string)
	for k, v := range parsed {
		if list, ok := v.([]any); ok {
			var strList []string
			for _, item := range list {
				strList = append(strList, fmt.Sprint(item))
			}
			attrs[k] = strList
		} else if valStr, ok := v.(string); ok {
			attrs[k] = []string{valStr}
		} else {
			attrs[k] = []string{fmt.Sprint(v)}
		}
	}
	return attrs, nil
}
