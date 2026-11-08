/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// A field path is written the way a person would say it: properties joined
// by dots, "[]" for the elements of a list, "{}" for the values of a map, and
// a trailing ".*" for "anything else under here".
//
//	spec.apps[].addonPins
//	spec.quotas{}
//	spec.config.*

// Missing returns the field paths the built schema declares and the served
// one would prune, sorted. Empty means everything written with the built
// schema in mind survives a write to the cluster.
//
// The comparison is structural -- which fields exist, down through objects,
// lists and maps -- and nothing else: a changed type, pattern, default or
// description is not a pruned field and is not reported.
//
// A subtree the cluster serves with x-kubernetes-preserve-unknown-fields
// prunes nothing, so everything under it counts as present whatever the built
// schema declares there. The reverse is reported: where the built schema
// keeps unknown fields and the served one does not, whatever is written there
// beyond the fields the cluster names is dropped.
//
// A missing subtree is reported once, at its root, not once per leaf.
func Missing(built, served *apiextensionsv1.JSONSchemaProps) []string {
	var out []string
	if built == nil {
		return nil
	}
	if served == nil {
		return []string{"*"}
	}
	missing("", built, served, &out)
	sort.Strings(out)
	return out
}

func preserves(s *apiextensionsv1.JSONSchemaProps) bool {
	return s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func missing(path string, built, served *apiextensionsv1.JSONSchemaProps, out *[]string) {
	if preserves(served) {
		return
	}
	if preserves(built) {
		*out = append(*out, join(path, "*"))
	}

	names := make([]string, 0, len(built.Properties))
	for name := range built.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := built.Properties[name]
		if have, ok := served.Properties[name]; ok {
			missing(join(path, name), &want, &have, out)
			continue
		}
		// Served as a map where it was built as an object: each key is kept
		// and held to the map's value schema.
		if ap := served.AdditionalProperties; ap != nil {
			if ap.Schema != nil {
				missing(join(path, name), &want, ap.Schema, out)
				continue
			}
			if ap.Allows {
				continue
			}
		}
		*out = append(*out, join(path, name))
	}

	if built.Items != nil && built.Items.Schema != nil {
		switch {
		case served.Items != nil && served.Items.Schema != nil:
			missing(path+"[]", built.Items.Schema, served.Items.Schema, out)
		default:
			*out = append(*out, path+"[]")
		}
	}

	if ap := built.AdditionalProperties; ap != nil && ap.Schema != nil {
		switch have := served.AdditionalProperties; {
		case have != nil && have.Schema != nil:
			missing(path+"{}", ap.Schema, have.Schema, out)
		case have != nil && have.Allows:
		default:
			*out = append(*out, path+"{}")
		}
	}
}

// under reports whether path is the field at root or one beneath it. A path
// ending in ".*" is a field like any other here: "whatever else is written
// under this node", which only that same path, or the node's whole subtree
// going missing, covers.
func under(path, root string) bool {
	if root == "*" {
		return true
	}
	if len(path) < len(root) || path[:len(root)] != root {
		return false
	}
	if len(path) == len(root) {
		return true
	}
	switch path[len(root)] {
	case '.', '[', '{':
		return true
	}
	return false
}
