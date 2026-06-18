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
