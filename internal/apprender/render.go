/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Rendered is one render's answer.
type Rendered struct {
	// Objects are what the app is installed with. Cluster-scoped ones (the
	// releases, the realm's objects, the store's) carry no namespace.
	Objects []*unstructured.Unstructured
	// ObserveOnly are objects that are read and never created, changed or
	// deleted: a Service alias over one the chart makes itself.
	ObserveOnly []*unstructured.Unstructured
}

// Render renders one app.
func Render(in *Input) (*Rendered, error) {
	if in.Profile == nil {
		return nil, fmt.Errorf("no profile to render %s from", in.App)
	}
	out := &Rendered{}
	for _, area := range areas {
		if err := area(in, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// areas render one part each, in the order the Composition rendered them.
var areas = []func(*Input, *Rendered) error{
	renderValues,
	renderIdentity,
	renderHooks,
	renderExtensions,
}

func (r *Rendered) add(objs ...*unstructured.Unstructured) {
	r.Objects = append(r.Objects, objs...)
}

// NamespacedKinds are the kinds a render makes in the app's own namespace.
var NamespacedKinds = []schema.GroupVersionKind{
	{Group: "", Version: "v1", Kind: "ConfigMap"},
	{Group: "", Version: "v1", Kind: "Service"},
	{Group: "batch", Version: "v1", Kind: "Job"},
	{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"},
}

// Consulted are the objects a render reads as they are now besides the ones
// it renders: an object that goes on being rendered once it exists, which is
// the client's optional scopes after the app stops asking for "mailbox".
func Consulted(in *Input) []*unstructured.Unstructured {
	scopes := &unstructured.Unstructured{}
	scopes.SetAPIVersion("openidclient.keycloak.crossplane.io/v1alpha1")
	scopes.SetKind("ClientOptionalScopes")
	scopes.SetName(newIdentityScope(in).name("keycloak-optional-scopes"))
	return []*unstructured.Unstructured{scopes}
}
