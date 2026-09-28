/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package catalogue

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResolveTenantAppProfile returns the ComponentProfile name for a tenant app
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
func ResolveTenantAppProfile(ctx context.Context, c client.Client, app gentianov1alpha1.TenantApp) (string, error) {
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
