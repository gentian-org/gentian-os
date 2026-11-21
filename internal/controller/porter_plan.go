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
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/porter"
)

// The porter's plan (internal/porter): what the tenant's file dialog serves,
// written where it reads it. The tenant's file home -- the provider every
// consumer of file-store is bound to, where its profile says where a
// person's files are -- and every installed app that asks the dialog
// (integrations[].dialog), with the origins of its pages and the uses it was
// granted. An app is in the plan exactly while the tenant's administrator
// grants it a use of the dialog (dialog:<use>, netpolicy.DialogCapability).
const (
	// PorterPlanName is the ConfigMap the plan is in, in the tenant's
	// namespace.
	PorterPlanName = "porter-plan"
	// PorterPlanKey is its one key.
	PorterPlanKey = "plan.json"

	fileStoreContract = "file-store"
)

// porterPlan is the tenant's plan, or nil where the tenant has no porter
// placed.
func porterPlan(tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput, zone addresses.Zone) *porter.Plan {
	placed := false
	for _, name := range in.Placed {
		if name == gentianov1alpha1.PorterProfile {
			placed = true
		}
	}
	if !placed {
		return nil
	}
	installed := make([]string, 0, len(in.Apps))
	for _, app := range in.Apps {
		installed = append(installed, app.Profile)
	}
	sort.Strings(installed)

	plan := &porter.Plan{Apps: []porter.App{}}
	providers := map[string][]string{}
	for _, name := range installed {
		if p := in.Profiles[name]; p != nil && providedEndpointOf(p, fileStoreContract) != nil {
			providers[fileStoreContract] = append(providers[fileStoreContract], name)
		}
	}
	if home := homeOf(tenant, providers, fileStoreContract); home != "" {
		plan.Home = fileHomeOf(home, in.Profiles[home], in.Namespace, zone)
	}

	// What of the dialog each app was granted: the dialog:<use>
	// capabilities of its file-store binding the tenant's administrator
	// granted.
	granted := map[string]map[string]bool{}
	for _, ib := range in.Bindings {
		if ib.Spec.Contract != fileStoreContract || ib.DeletionTimestamp != nil {
			continue
		}
		app := ib.Spec.Consumer.App
		for _, c := range netpolicy.EffectiveContractCapabilities(ib, in.Grants[app]) {
			if granted[app] == nil {
				granted[app] = map[string]bool{}
			}
			granted[app][c] = true
		}
	}
	for _, name := range installed {
		p := in.Profiles[name]
		if p == nil || granted[name] == nil {
			continue
		}
		var uses []porter.Use
		for _, i := range p.Spec.Integrations {
			if i.Contract != fileStoreContract {
				continue
			}
			for _, u := range i.Dialog {
				if granted[name][netpolicy.DialogCapability(string(u))] {
					uses = append(uses, porter.Use(u))
				}
			}
		}
		origins := pageOrigins(name, p, zone)
		if len(uses) == 0 || len(origins) == 0 {
			continue
		}
		plan.Apps = append(plan.Apps, porter.App{Profile: name, Origins: origins, Uses: uses})
	}
	return plan
}

// fileHomeOf is the home as the porter is told it, or nil for a provider
// that does not say where a person's files are or whose open address names
// no gateway entry of its own.
func fileHomeOf(name string, p *gentianov1alpha1.ComponentProfile, namespace string, zone addresses.Zone) *porter.Home {
	for _, c := range p.Spec.Provides {
		if c.Name != fileStoreContract || c.Endpoint == nil || c.Files == nil {
			continue
		}
		for i := range p.Spec.Expose {
			e := &p.Spec.Expose[i]
			if e.Name != c.Files.Open.Entry || e.Surface != gentianov1alpha1.SurfaceGateway {
				continue
			}
			host := addresses.Host(zone, name, e)
			if host == "" {
				return nil
			}
			return &porter.Home{
				Profile:    name,
				URL:        contractURL(c.Endpoint, namespace),
				Root:       c.Files.Root,
				IDProperty: c.Files.IDProperty,
				Open:       "https://" + host + c.Files.Open.Path,
			}
		}
	}
	return nil
}

// pageOrigins are the origins an app's pages are served from: its gateway
// entries' hosts, sorted.
func pageOrigins(name string, p *gentianov1alpha1.ComponentProfile, zone addresses.Zone) []string {
	set := map[string]bool{}
	for i := range p.Spec.Expose {
		e := &p.Spec.Expose[i]
		if e.Surface != gentianov1alpha1.SurfaceGateway {
			continue
		}
		if host := addresses.Host(zone, name, e); host != "" {
			set["https://"+host] = true
		}
	}
	return sortedKeys(set)
}

// ensurePorterPlan writes the tenant's plan, or removes it where the tenant
// has no porter.
func (r *TenantReconciler) ensurePorterPlan(ctx context.Context, tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) error {
	zone := addresses.ZoneOf(tenant, r.KernelDomain, r.TenancyMode, r.KernelRealm)
	plan := porterPlan(tenant, in, zone)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: PorterPlanName, Namespace: in.Namespace}}
	if plan == nil {
		err := r.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, cm)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return client0(r.Delete(ctx, cm))
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{PorterPlanKey: string(raw)}
		return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("write the porter's plan: %w", err)
	}
	return nil
}
