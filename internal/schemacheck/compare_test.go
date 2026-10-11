/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func schemaOf(t *testing.T, text string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	var s apiextensionsv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(text), &s); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	return &s
}

// The schema the software was built with, in every test below: an object, a
// list of objects, a map of objects, and a subtree that keeps whatever is put
// in it.
const builtSchema = `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef:
        type: object
        properties:
          name: {type: string}
          digest: {type: string}
      apps:
        type: array
        items:
          type: object
          properties:
            profile: {type: string}
            digest: {type: string}
            addonPins:
              type: array
              items:
                type: object
                properties:
                  name: {type: string}
                  digest: {type: string}
      quotas:
        type: object
        additionalProperties:
          type: object
          properties:
            limit: {type: string}
            request: {type: string}
      config:
        type: object
        x-kubernetes-preserve-unknown-fields: true
        properties:
          replicas: {type: integer}
  status:
    type: object
    properties:
      phase: {type: string}
`

func TestMissingFields(t *testing.T) {
	cases := []struct {
		name   string
		served string
		want   []string
	}{
		{
			name:   "the same schema prunes nothing",
			served: builtSchema,
		},
		{
			name: "a field the cluster has and the software does not is nobody's loss",
			served: builtSchema + `
      somethingNewer: {type: string}
`,
		},
		{
			name: "a leaf missing under a nested object",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef:
        type: object
        properties:
          name: {type: string}
      apps: {type: array, items: {type: object, x-kubernetes-preserve-unknown-fields: true}}
      quotas: {type: object, x-kubernetes-preserve-unknown-fields: true}
      config: {type: object, x-kubernetes-preserve-unknown-fields: true}
  status: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.profileRef.digest"},
		},
		{
			name: "a field missing from the elements of a list, and from a list inside them",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef: {type: object, x-kubernetes-preserve-unknown-fields: true}
      apps:
        type: array
        items:
          type: object
          properties:
            profile: {type: string}
            addonPins:
              type: array
              items:
                type: object
                properties:
                  name: {type: string}
      quotas: {type: object, x-kubernetes-preserve-unknown-fields: true}
      config: {type: object, x-kubernetes-preserve-unknown-fields: true}
  status: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.apps[].addonPins[].digest", "spec.apps[].digest"},
		},
		{
			name: "a whole subtree missing is reported once, at its root",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef: {type: object, x-kubernetes-preserve-unknown-fields: true}
      apps:
        type: array
        items:
          type: object
          properties:
            profile: {type: string}
            digest: {type: string}
      config: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.apps[].addonPins", "spec.quotas", "status"},
		},
		{
			name: "a field missing from the values of a map",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef: {type: object, x-kubernetes-preserve-unknown-fields: true}
      apps: {type: array, items: {type: object, x-kubernetes-preserve-unknown-fields: true}}
      quotas:
        type: object
        additionalProperties:
          type: object
          properties:
            limit: {type: string}
      config: {type: object, x-kubernetes-preserve-unknown-fields: true}
  status: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.quotas{}.request"},
		},
		{
			name: "a subtree the cluster keeps unknown fields in prunes nothing, whatever it declares",
			served: `
type: object
properties:
  spec:
    type: object
    x-kubernetes-preserve-unknown-fields: true
    properties:
      displayName: {type: string}
  status:
    type: object
    x-kubernetes-preserve-unknown-fields: true
`,
		},
		{
			name: "a subtree the software keeps unknown fields in and the cluster does not",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef: {type: object, x-kubernetes-preserve-unknown-fields: true}
      apps: {type: array, items: {type: object, x-kubernetes-preserve-unknown-fields: true}}
      quotas: {type: object, x-kubernetes-preserve-unknown-fields: true}
      config:
        type: object
        properties:
          replicas: {type: integer}
  status: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.config.*"},
		},
		{
			name: "a list the cluster serves without a schema for its elements",
			served: `
type: object
properties:
  spec:
    type: object
    properties:
      displayName: {type: string}
      profileRef: {type: object, x-kubernetes-preserve-unknown-fields: true}
      apps: {type: string}
      quotas: {type: object, x-kubernetes-preserve-unknown-fields: true}
      config: {type: object, x-kubernetes-preserve-unknown-fields: true}
  status: {type: object, x-kubernetes-preserve-unknown-fields: true}
`,
			want: []string{"spec.apps[]"},
		},
	}
	built := schemaOf(t, builtSchema)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Missing(built, schemaOf(t, c.served))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("missing fields:\n got %v\nwant %v", got, c.want)
			}
		})
	}
}

func TestNoServedSchemaIsEverythingMissing(t *testing.T) {
	if got := Missing(schemaOf(t, builtSchema), nil); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("got %v", got)
	}
}

func TestUnder(t *testing.T) {
	cases := []struct {
		path, root string
		want       bool
	}{
		{"spec.apps[].addonPins", "spec.apps[].addonPins", true},
		{"spec.apps[].addonPins[].digest", "spec.apps[].addonPins", true},
		{"spec.apps[].addonPinsExtra", "spec.apps[].addonPins", false},
		{"spec.apps[].digest", "spec.apps[].addonPins", false},
		{"spec.quotas{}.request", "spec.quotas", true},
		{"spec.config.*", "spec.config.*", true},
		{"spec.config.replicas", "spec.config.*", false},
		{"spec.config.*", "spec.config", true},
		{"anything", "*", true},
	}
	for _, c := range cases {
		if got := under(c.path, c.root); got != c.want {
			t.Errorf("under(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
}
