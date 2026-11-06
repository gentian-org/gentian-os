/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ExternalSecretGVK is the kind of an ExternalSecret. It is used
// unstructured rather than through ESO's Go types: adding external-secrets to
// go.mod for a handful of fields would couple this build to an API version
// that has already moved once (v1beta1 stopped being served).
var ExternalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}

// ExternalSecret is an ExternalSecret named name in namespace that reads data
// through store every hour into the Secret target describes.
//
// No data is an explicit null, as a probe of a credential with no fields
// was always written: a typed empty list would read as different from what
// the API server gives back.
func ExternalSecret(name, namespace, store string, target map[string]any, data []any) *unstructured.Unstructured {
	spec := map[string]any{
		"refreshInterval": "1h",
		"secretStoreRef":  StoreRef(store),
		"target":          target,
		"data":            nil,
	}
	if data != nil {
		spec["data"] = data
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ExternalSecretGVK.GroupVersion().String(),
		"kind":       ExternalSecretGVK.Kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
}

// RemoteRef is one entry of an ExternalSecret's data: the Secret's secretKey,
// read from property of the record at path.
func RemoteRef(secretKey, path, property string) map[string]any {
	return map[string]any{"secretKey": secretKey, "remoteRef": map[string]any{"key": path, "property": property}}
}
