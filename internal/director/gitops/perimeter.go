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
	"os"

	"sigs.k8s.io/yaml"
)

// Who approves what a tenant publishes to the internet, beyond the two who
// always may.
//
// A public address is approved by a person who holds can_expose on the
// tenant: a member of the tenant's perimeter group, or the cluster's
// administrator while the tenant is operated by its cluster. Whether the
// tenant's own administrators hold it too is a switch on the tenant's
// manifest, spec.perimeter.adminsApprove, off unless the cluster's
// administrator turns it on. The director writes the switch and nothing
// else: the operator projects it into the rights store, where the grant is
// held while the manifest says so and removed when it stops.

// ErrPerimeterPlatformTenant is the switch asked for the platform tenant,
// whose administrators are the cluster's and approve there already. The
// message is what a person reads: the API hands it on unchanged.
var ErrPerimeterPlatformTenant = errors.New(
	"this is the platform tenant: its administrators are the cluster's, " +
		"who approve public addresses in it already. There is nothing to switch on")

// parseTenantPerimeter reads spec.perimeter.adminsApprove from a tenant
// manifest's text.
func parseTenantPerimeter(text string) (bool, error) {
	var doc struct {
		Spec struct {
			Perimeter *struct {
				AdminsApprove bool `json:"adminsApprove"`
			} `json:"perimeter"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return false, fmt.Errorf("parse tenant manifest: %w", err)
	}
	return doc.Spec.Perimeter != nil && doc.Spec.Perimeter.AdminsApprove, nil
}

// TenantAdminsApprove reads whether a tenant's own administrators may approve
// its public addresses.
func (g *GitOps) TenantAdminsApprove(ctx context.Context, tenant string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	return parseTenantPerimeter(string(raw))
}

// SetTenantAdminsApprove turns on or off whether a tenant's own
// administrators may approve its public addresses. Turning it off withdraws
// nothing that was approved: what is published stays published until
// somebody who may approve withdraws it.
func (g *GitOps) SetTenantAdminsApprove(ctx context.Context, tenant string, approve bool, meta Meta) (Result, error) {
	verb := "let the tenant's administrators approve public addresses"
	if !approve {
		verb = "stop the tenant's administrators approving public addresses"
	}
	msg := fmt.Sprintf("feat(%s): %s (via %s)", tenant, verb, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantPerimeter(text)
		if err != nil {
			return text, "", false, err
		}
		// Decided on the text the commit is made from, so a manifest that
		// changed a moment ago is judged as it is now.
		if approve && adoptsAnotherRealm(text, tenant) {
			return text, "", false, ErrPerimeterPlatformTenant
		}
		if have == approve {
			return text, "unchanged", false, nil
		}
		out, err := setTenantPerimeter(text, approve)
		if err != nil {
			return text, "", false, err
		}
		return out, "updated", true, nil
	})
}

// setTenantPerimeter writes spec.perimeter into a tenant manifest's text, or
// removes it when the switch is off, and checks that what it wrote reads back
// as what was meant.
func setTenantPerimeter(text string, approve bool) (string, error) {
	var block []string
	if approve {
		block = []string{"  perimeter:", "    adminsApprove: true"}
	}
	out, err := setSpecBlock(text, []string{"perimeter"}, block)
	if err != nil {
		return "", err
	}
	back, err := parseTenantPerimeter(out)
	if err != nil {
		return "", fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if back != approve {
		return "", errors.New("the edited manifest does not read back as written")
	}
	return out, nil
}
