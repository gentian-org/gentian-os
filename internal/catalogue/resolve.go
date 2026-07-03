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

// ResolveTenantAppProfile returns the AppProfile CR name for a tenant app entry.
// Uses profile when set; otherwise resolves profileRef against all AppProfiles in the cluster.
func ResolveTenantAppProfile(ctx context.Context, c client.Client, app gentianov1alpha1.TenantApp) (string, error) {
	if app.Profile != "" {
		return app.Profile, nil
	}
	if app.ProfileRef == nil {
		return "", fmt.Errorf("tenant app has neither profile nor profileRef")
	}

	list := &gentianov1alpha1.AppProfileList{}
	if err := c.List(ctx, list); err != nil {
		return "", fmt.Errorf("list AppProfiles: %w", err)
	}
	name, ok := gentianov1alpha1.ResolveProfileReference(list.Items, *app.ProfileRef)
	if !ok {
		return "", fmt.Errorf("profileRef did not resolve to a unique AppProfile")
	}
	return name, nil
}
