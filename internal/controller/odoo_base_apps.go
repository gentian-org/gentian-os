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

const odooFreeBaseProfile = "odoo-free-base"

// ensureImplicitBaseApps injects required base profiles (e.g. odoo-free-base when
// an odoo module profile is installed) into tenant.Spec.Apps before provisioning.
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
		if profile.Spec.DeploymentRole != gentianov1alpha1.DeploymentRoleModule {
			continue
		}
		base := profile.Spec.RequiresBaseProfile
		if base == "" && profile.Spec.Family == "odoo" {
			base = odooFreeBaseProfile
		}
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
