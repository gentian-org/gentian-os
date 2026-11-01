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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ErrInvalidMailMode is a mail mode the Tenant kind does not know.
var ErrInvalidMailMode = errors.New("not a mail mode")

// MailModes is what spec.mail.mode may say, as the Tenant kind declares them.
var MailModes = []string{
	string(gentianov1alpha1.MailModeSelfhosted),
	string(gentianov1alpha1.MailModeExternal),
	string(gentianov1alpha1.MailModeTransportOnly),
	string(gentianov1alpha1.MailModeDisabled),
}

// SetTenantMailMode writes spec.mail.mode into the tenant's manifest: one
// commit, or "unchanged" where it says so already. What the mode means is
// the operator's (docs/concepts/mail.md §2); the director only puts the word
// where the operator reads it. A tenant that names no mode gets the cluster's
// default, so a mode is written rather than removed: the manifest then says
// what was decided.
func (g *GitOps) SetTenantMailMode(ctx context.Context, tenant, mode string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	known := false
	for _, m := range MailModes {
		if m == mode {
			known = true
			break
		}
	}
	if !known {
		return Result{}, fmt.Errorf("%w: %q; one of %s", ErrInvalidMailMode, mode, strings.Join(MailModes, ", "))
	}
	msg := fmt.Sprintf("feat(%s): mail mode %s (via %s)", tenant, mode, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		out, changed, err := setKeyPath(text, "spec", []string{"mail", "mode"}, scalarLeaf(mode))
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		return out, "updated", true, nil
	})
}
