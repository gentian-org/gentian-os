/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package v1alpha1

import "fmt"

// PositionAdministers reports whether the tenant's position name holds the
// role admin on the tenant or on one of its apps, itself or through a
// position it includes: the positions of the tenant's and of its apps'
// administrators, and a position of spec.positions that names such a role.
//
// Positions given without an administrator -- at sign-up, at a sign-in
// from another tenant, by a component the tenant names -- are never one of
// these.
func (t *Tenant) PositionAdministers(name string) bool {
	declared := make(map[string]*TenantPosition, len(t.Spec.Positions))
	for i := range t.Spec.Positions {
		declared[t.Spec.Positions[i].Name] = &t.Spec.Positions[i]
	}
	seen := map[string]bool{}
	queue := []string{name}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		if p == "tenant-administrator" || p == "app-administrator" {
			return true
		}
		pos := declared[p]
		if pos == nil {
			continue
		}
		for _, r := range pos.Roles {
			if r.Role == "admin" {
				return true
			}
		}
		queue = append(queue, pos.Includes...)
	}
	return false
}

// AdministrationGivenUnasked names the first position the tenant would
// give without an administrator that administers (PositionAdministers):
// one of spec.signUp.positions, spec.signInFrom[].positions or
// spec.positionGivers[].positions. Empty when there is none.
func (t *Tenant) AdministrationGivenUnasked() string {
	if t.Spec.SignUp != nil {
		for _, p := range t.Spec.SignUp.Positions {
			if t.PositionAdministers(p) {
				return fmt.Sprintf("spec.signUp.positions names %s", p)
			}
		}
	}
	for _, from := range t.Spec.SignInFrom {
		for _, p := range from.Positions {
			if t.PositionAdministers(p) {
				return fmt.Sprintf("spec.signInFrom (%s) names %s", from.Tenant, p)
			}
		}
	}
	for _, g := range t.Spec.PositionGivers {
		for _, p := range g.Positions {
			if t.PositionAdministers(p) {
				return fmt.Sprintf("spec.positionGivers (%s/%s) names %s", g.Tenant, g.Component, p)
			}
		}
	}
	return ""
}
