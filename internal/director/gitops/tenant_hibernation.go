/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ErrPlatformTenantSleeps is a sleep asked of the platform tenant, through
// which the cluster is administered.
var ErrPlatformTenantSleeps = errors.New("the platform tenant is not put to sleep: the cluster is administered through it")

// Hibernation is spec.hibernation of a tenant: whether it is asleep, and
// until when it is kept awake although it is.
type Hibernation struct {
	Asleep     bool       `json:"asleep"`
	AwakeUntil *time.Time `json:"awakeUntil,omitempty"`
}

// asleepAt is whether a tenant declared this way sleeps at the given time,
// as the operator reads it (Tenant.Asleep).
func (h Hibernation) asleepAt(now time.Time) bool {
	return h.Asleep && (h.AwakeUntil == nil || !now.Before(*h.AwakeUntil))
}

// same is whether two declarations say the same. A tenant that is not asleep
// is awake whatever else the block carries.
func (h Hibernation) same(o Hibernation) bool {
	if !h.Asleep || !o.Asleep {
		return h.Asleep == o.Asleep
	}
	if h.AwakeUntil == nil || o.AwakeUntil == nil {
		return h.AwakeUntil == o.AwakeUntil
	}
	return h.AwakeUntil.Equal(*o.AwakeUntil)
}

// declaredHibernation reads spec.hibernation from a tenant's manifest. A
// manifest without the block declares a tenant that is awake.
func declaredHibernation(manifest string) (Hibernation, error) {
	var doc struct {
		Spec struct {
			Hibernation Hibernation `json:"hibernation"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return Hibernation{}, err
	}
	return doc.Spec.Hibernation, nil
}

// SetTenantHibernation writes spec.hibernation into the tenant's manifest:
// one commit, or "unchanged" where the manifest says so already. What sleep
// does to a tenant is the operator's; the director only records that it was
// asked for, by whom, and until when the tenant stays awake. The platform
// tenant is refused.
func (g *GitOps) SetTenantHibernation(ctx context.Context, tenant string, want Hibernation, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if tenant == platformTenant {
		return Result{}, ErrPlatformTenantSleeps
	}
	if !want.Asleep {
		want.AwakeUntil = nil
	}
	if want.AwakeUntil != nil {
		until := want.AwakeUntil.UTC().Truncate(time.Second)
		want.AwakeUntil = &until
	}
	var msg string
	switch {
	case !want.Asleep:
		msg = fmt.Sprintf("feat(%s): wake (via %s)", tenant, meta.actor())
	case want.AwakeUntil != nil:
		msg = fmt.Sprintf("feat(%s): awake until %s (via %s)", tenant, want.AwakeUntil.Format(time.RFC3339), meta.actor())
	default:
		msg = fmt.Sprintf("feat(%s): put to sleep (via %s)", tenant, meta.actor())
	}
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		if adoptsAnotherRealm(text, tenant) {
			return text, "", false, ErrPlatformTenantSleeps
		}
		declared, err := declaredHibernation(text)
		if err != nil {
			return text, "", false, err
		}
		if declared.same(want) {
			return text, "unchanged", false, nil
		}
		out, changed, err := setKeyPath(text, "spec", []string{"hibernation"}, hibernationLeaf(want))
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		return out, "updated", true, nil
	})
}

// hibernationLeaf writes spec.hibernation as a block, replacing the one
// there whole, so a tenant put to sleep keeps no awakeUntil line behind.
type hibernationLeaf Hibernation

func (h hibernationLeaf) render(indent int, key string) []string {
	pad := strings.Repeat(" ", indent)
	out := []string{pad + key + ":", fmt.Sprintf("%s  asleep: %t", pad, h.Asleep)}
	if h.AwakeUntil != nil {
		out = append(out, fmt.Sprintf("%s  awakeUntil: %q", pad, h.AwakeUntil.Format(time.RFC3339)))
	}
	return out
}

func (hibernationLeaf) extent(lines []string, at, indent int) int {
	return listLeaf(nil).extent(lines, at, indent)
}

// same is never asked to decide: SetTenantHibernation compares what the
// manifest declares before it writes.
func (hibernationLeaf) same([]string, int, int) bool { return false }
