// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
)

// ensureImplicitBaseApps injects profiles listed in gentianos.io/requires-profile when
// a module profile (gentianos.io/deployment-role=module) is installed. Generic
// catalogue semantics — app-specific install Jobs read extraValues in compositions.
func (r *TenantReconciler) ensureImplicitBaseApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if len(tenant.Spec.Apps) == 0 {
		return ctrl.Result{}, nil
	}

	present := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		name, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		present[name] = struct{}{}
	}

	var prepend []gentianov1alpha1.TenantApp
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: profileName}, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", profileName, err)
		}
		if gentianov1alpha1.EffectiveDeploymentRole(profile) != gentianov1alpha1.ProfileDeploymentRoleModule {
			continue
		}
		base := gentianov1alpha1.ProfileRequiresProfile(profile)
		if base == "" {
			continue
		}
		if _, ok := present[base]; ok {
			continue
		}
		prepend = append(prepend, gentianov1alpha1.TenantApp{Profile: base})
		present[base] = struct{}{}
	}

	if len(prepend) == 0 {
		return ctrl.Result{}, nil
	}
	tenant.Spec.Apps = append(prepend, tenant.Spec.Apps...)
	if err := r.Update(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("inject implicit base apps: %w", err)
	}
	return ctrl.Result{Requeue: true}, nil
}
