/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's grants (spec.grants), as the AppGrants of its namespace.
//
// The director writes a grant into the Tenant, with who granted it and when;
// Argo CD syncs the Tenant; the operator makes each entry the AppGrant of its
// app in the tenant's namespace, which the bindings, the contract network
// policies and the platform's view read as they always did. An AppGrant made
// here whose entry is gone is removed, and with it what it permitted. Who
// granted it and when are on the AppGrant as annotations, for whoever reads
// it on the cluster.
const (
	// appGrantDeclaredLabel marks an AppGrant this makes from the Tenant.
	appGrantDeclaredLabel = "gentianos.io/declared-in"
	appGrantDeclaredValue = "tenant-grants"

	appGrantGrantedByAnnotation = "gentianos.io/granted-by"
	appGrantGrantedAtAnnotation = "gentianos.io/granted-at"
	appGrantReasonAnnotation    = "gentianos.io/grant-reason"
)

// grantAppName is what an app's name in a grant is: a DNS label, as the
// AppGrant's object name it becomes.
var grantAppName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ensureAppGrants keeps the tenant namespace's AppGrants equal to the
// tenant's grants.
func (r *TenantReconciler) ensureAppGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	ns := tenantNamespaceName(tenant)
	declared := map[string]bool{}
	for i := range tenant.Spec.Grants {
		g := &tenant.Spec.Grants[i]
		if !grantAppName.MatchString(g.App) {
			continue
		}
		declared[g.App] = true
		ag := &gentianov1alpha1.AppGrant{ObjectMeta: metav1.ObjectMeta{Name: g.App, Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ag, func() error {
			if ag.Labels == nil {
				ag.Labels = map[string]string{}
			}
			ag.Labels[managedByLabel] = managedByValue
			ag.Labels[tenantLabel] = tenant.Name
			ag.Labels[appGrantDeclaredLabel] = appGrantDeclaredValue
			if ag.Annotations == nil {
				ag.Annotations = map[string]string{}
			}
			setOrDrop(ag.Annotations, appGrantGrantedByAnnotation, g.GrantedBy)
			at := ""
			if g.GrantedAt != nil {
				at = g.GrantedAt.UTC().Format(time.RFC3339)
			}
			setOrDrop(ag.Annotations, appGrantGrantedAtAnnotation, at)
			setOrDrop(ag.Annotations, appGrantReasonAnnotation, g.Reason)
			ag.Spec = *g.AppGrantSpec.DeepCopy()
			return controllerutil.SetControllerReference(tenant, ag, r.Scheme)
		}); err != nil {
			return fmt.Errorf("AppGrant %s/%s: %w", ns, g.App, err)
		}
	}
	list := &gentianov1alpha1.AppGrantList{}
	if err := r.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{appGrantDeclaredLabel: appGrantDeclaredValue}); err != nil {
		return fmt.Errorf("list AppGrants in %s: %w", ns, err)
	}
	for i := range list.Items {
		if declared[list.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove AppGrant %s/%s, withdrawn: %w", ns, list.Items[i].Name, err)
		}
	}
	return nil
}

func setOrDrop(m map[string]string, key, value string) {
	if value == "" {
		delete(m, key)
		return
	}
	m[key] = value
}
