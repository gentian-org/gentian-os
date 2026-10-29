/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package addresses

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// AliasHost is the second address of one published surface, directly under
// the cluster's own domain, or "" where it has none that is honored.
//
// The director refuses an alias that could not be honored before it is
// written, so this is the same rule asked again where the address is
// published: what is in git may have been written by hand, or have become
// wrong since -- a tenant created under the alias's name by another route.
// Not honoured: on a single-tenancy cluster, where a tenant's addresses are
// at that level already; a name the platform keeps; a name a tenant is
// called, since <name>.<cluster> is that tenant's own address; and, where
// two surfaces carry one alias, every one but the first that was given it.
// Whether the surface itself is published is not asked here.
func AliasHost(in Inputs, tenant, install, exposure string) string {
	label, setAt := "", ""
	for i := range in.Tenants {
		if in.Tenants[i].Name != tenant {
			continue
		}
		for _, e := range in.Tenants[i].Spec.Exposures {
			if e.Install == install && e.ExposureName == exposure {
				label, setAt = e.Alias, aliasSetAt(e)
			}
		}
	}
	if label == "" || in.KernelDomain == "" {
		return ""
	}
	if gentianov1alpha1.NormalizeTenancyMode(in.TenancyMode) == gentianov1alpha1.TenancyModeSingle {
		return ""
	}
	if hostnames.Kept(label) != nil {
		return ""
	}
	mine := aliasOrder{setAt, tenant, install, exposure}
	for i := range in.Tenants {
		other := &in.Tenants[i]
		if other.Name == label {
			return ""
		}
		for _, e := range other.Spec.Exposures {
			if e.Alias != label {
				continue
			}
			if (aliasOrder{aliasSetAt(e), other.Name, e.Install, e.ExposureName}).before(mine) {
				return ""
			}
		}
	}
	return label + "." + in.KernelDomain
}

func aliasSetAt(e gentianov1alpha1.TenantExposure) string {
	if e.AliasSetAt == nil {
		return ""
	}
	return e.AliasSetAt.UTC().Format("2006-01-02T15:04:05Z")
}

// aliasOrder decides between two surfaces that carry one alias: the one
// given it first, and by name where that does not tell them apart, so that
// the operator and the director always name the same one.
type aliasOrder [4]string

func (a aliasOrder) before(b aliasOrder) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
