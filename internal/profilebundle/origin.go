/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Where a materialised profile came from.
//
// A ComponentProfile is cluster-scoped, and a catalogue need not be: a tenant
// may have one only it sees. So the profile itself has to say whose it is, or
// a profile one tenant published would be installable by every other tenant
// that learned its name. The director writes the origin with the bundle when
// it materialises the profile, and two things read it: the director, before it
// commits an install, and the operator, before it rolls one out. Two checks,
// because the object is visible cluster-wide whatever the director refused.

// OriginAnnotation says which catalogue a materialised profile was fetched
// from: "cluster/<source>" for a catalogue every tenant sees, or
// "tenant/<tenant>/<source>" for one that belongs to a tenant.
//
// It is the director's to write, beside the bundle, and never a source's to
// state. A profile without it is one nobody recorded an origin for -- shipped
// by the platform's chart, scaffolded by the installer, or materialised before
// origins were recorded -- and it belongs to no tenant.
const OriginAnnotation = "gentianos.io/catalogue-origin"

// ReasonOtherTenant is the condition reason of a Component whose profile
// belongs to another tenant.
const ReasonOtherTenant = "ProfileOfAnotherTenant"

// Origin is a parsed OriginAnnotation.
type Origin struct {
	// Tenant is the tenant the catalogue belongs to; empty for a catalogue
	// of the whole cluster.
	Tenant string
	// Source is the catalogue's name.
	Source string
}

// ClusterOrigin is the origin of a profile from a catalogue of the cluster.
func ClusterOrigin(source string) string { return "cluster/" + source }

// TenantOrigin is the origin of a profile from a tenant's own catalogue.
func TenantOrigin(tenant, source string) string { return "tenant/" + tenant + "/" + source }

// ParseOrigin reads an origin. Empty is no origin and no error. Anything that
// is neither form is an error, and a caller deciding whether a tenant may use
// the profile treats an error as "no".
func ParseOrigin(value string) (Origin, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Origin{}, nil
	}
	parts := strings.Split(value, "/")
	switch {
	case len(parts) == 2 && parts[0] == "cluster" && parts[1] != "":
		return Origin{Source: parts[1]}, nil
	case len(parts) == 3 && parts[0] == "tenant" && parts[1] != "" && parts[2] != "":
		return Origin{Tenant: parts[1], Source: parts[2]}, nil
	}
	return Origin{}, fmt.Errorf("%q is not cluster/<source> or tenant/<tenant>/<source>", value)
}

// UsableBy reports whether a profile of this origin may be installed into
// tenant: always, unless the origin is another tenant's own catalogue or
// cannot be read.
func UsableBy(value, tenant string) bool {
	origin, err := ParseOrigin(value)
	if err != nil {
		return false
	}
	return origin.Tenant == "" || origin.Tenant == tenant
}

// OwnedByAnother answers the refusal for a profile that tenant may not have
// rolled out, or nil when it may.
func OwnedByAnother(profile *gentianov1alpha1.ComponentProfile, tenant string) *Refusal {
	value := profile.Annotations[OriginAnnotation]
	if UsableBy(value, tenant) {
		return nil
	}
	if _, err := ParseOrigin(value); err != nil {
		return &Refusal{Reason: ReasonOtherTenant, Message: fmt.Sprintf(
			"ComponentProfile %q states an origin that cannot be read (annotation %s: %v), so it is not known whose it is",
			profile.Name, OriginAnnotation, err)}
	}
	return &Refusal{Reason: ReasonOtherTenant, Message: fmt.Sprintf(
		"ComponentProfile %q comes from another tenant's own catalogue and can be installed only in that tenant, not in %s",
		profile.Name, tenant)}
}
