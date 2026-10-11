/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResolveTenantComponentProfile returns the ComponentProfile name for a tenant app
// entry.
//
// A name, or a profileRef naming one. Resolving a profileRef by IDENTITY —
// family, catalogue version, edition — is gone: AD-3 moves that metadata to
// the store's listing, outside the cluster, so there is nothing here to match
// against. The store resolves an identity to a name and the tenant manifest
// carries the name, which is what every tenant already does.
//
// A profileRef that carries an identity is therefore refused rather than
// silently matching nothing: an install that quietly resolved to no profile
// would look like an app that never arrived.
func ResolveTenantComponentProfile(ctx context.Context, c client.Client, app gentianov1alpha1.TenantApp) (string, error) {
	if app.Profile != "" {
		return app.Profile, nil
	}
	if app.ProfileRef == nil {
		return "", fmt.Errorf("tenant app has neither profile nor profileRef")
	}
	if app.ProfileRef.Name == "" {
		return "", fmt.Errorf(
			"profileRef resolves a catalogue identity, and the catalogue's metadata is the " +
				"store's now — name the profile, or let the store resolve it and commit the name")
	}

	// It still has to exist. A tenant naming a profile the cluster does not
	// have is the failure this catches early, rather than as a composition
	// that renders nothing.
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(ctx, client.ObjectKey{Name: app.ProfileRef.Name}, profile); err != nil {
		return "", fmt.Errorf("profileRef %q: %w", app.ProfileRef.Name, err)
	}
	return profile.Name, nil
}
