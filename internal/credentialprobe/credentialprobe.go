/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package credentialprobe says which credentials a person has to supply for
// an app are not set yet, by the probe each one's CredentialRequirement has.
//
// The operator renders, for each entry of a profile's
// spec.secrets.supplied, a CredentialRequirement and a probe: an
// ExternalSecret credreq-<requirement> in the control namespace that creates
// nothing and is Ready once every field is in the vault. The app's release
// waits for the same fields, so an app that is not up and has a probe that
// is not Ready is waiting for a person. Nothing here reads a value.
package credentialprobe

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

var externalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}

// Awaited is one supplied credential that is not set yet.
type Awaited struct {
	// Name is the entry's name in the profile; DisplayName what the
	// tenant's Admin Console shows it as, under Credentials.
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Requirement is the CredentialRequirement that asks for it.
	Requirement string `json:"requirement"`
}

// AwaitedSupplied answers the credentials the profile asks a person for
// under spec.secrets.supplied that are not set for this app of this tenant.
// A probe that cannot be read, or has not reported, counts as not set: the
// app waits for it all the same.
func AwaitedSupplied(ctx context.Context, c client.Reader, tenant, app string, profile *gentianov1alpha1.ComponentProfile) []Awaited {
	if profile == nil {
		return nil
	}
	var out []Awaited
	for _, s := range profile.SuppliedSecrets() {
		name := secrets.SuppliedRequirementName(tenant, app, s.Name)
		if Satisfied(ctx, c, name) {
			continue
		}
		out = append(out, Awaited{Name: s.Name, DisplayName: s.DisplayName, Requirement: name})
	}
	return out
}

// Satisfied reads ESO's verdict from the probe of the requirement.
func Satisfied(ctx context.Context, c client.Reader, requirement string) bool {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: meta.OperatorNamespace, Name: "credreq-" + requirement}, es); err != nil {
		return false
	}
	conds, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
	for _, raw := range conds {
		if cond, ok := raw.(map[string]any); ok && cond["type"] == "Ready" {
			return cond["status"] == "True"
		}
	}
	return false
}

// Text names the awaited credentials for a person, and where they are set:
// "the credential Payment keys (payments), which ...".
func Text(awaited []Awaited) string {
	if len(awaited) == 0 {
		return ""
	}
	names := ""
	for i, a := range awaited {
		if i > 0 {
			names += ", "
		}
		label := a.DisplayName
		if label == "" {
			label = a.Name
		}
		names += label + " (" + a.Name + ")"
	}
	what, it := "the credential ", "it"
	if len(awaited) > 1 {
		what, it = "the credentials ", "them"
	}
	return "waits for " + what + names + ", which a person supplies under Credentials in the tenant's Admin Console; no bundle carries " + it
}
