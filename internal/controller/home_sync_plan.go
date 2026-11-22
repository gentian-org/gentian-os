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
	"github.com/gentian-org/gentian-os/internal/homesync"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The home sync's plan (internal/homesync): what the tenant's home sync keeps
// equal, written where it reads it. The tenant's home per kind -- the
// provider every consumer of calendar-store and contacts-store is bound to,
// and the address its profile names for the contract -- and every installed
// app that declares homeSync, with its address and its mappings. It follows
// installs; it grants nothing, since what the service may do is the realm's
// answer to the tokens it asks for.
const (
	// HomeSyncPlanName is the ConfigMap the plan is in, in the tenant's
	// namespace.
	HomeSyncPlanName = "homesync-plan"
	// HomeSyncPlanKey is its one key.
	HomeSyncPlanKey = "plan.json"
)

// homeSyncPlan is the tenant's plan, or nil where the tenant has no home
// sync placed.
func homeSyncPlan(tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) *homesync.Plan {
	placed := false
	for _, name := range in.Placed {
		if name == gentianov1alpha1.HomeSyncProfile {
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

	plan := &homesync.Plan{ContactsBook: tenant.Spec.ContactsBook}
	homeOf := func(contract string) *homesync.HomeOf {
		var candidates []string
		for _, name := range installed {
			if p := in.Profiles[name]; p != nil && providedEndpointOf(p, contract) != nil {
				candidates = append(candidates, name)
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		home := candidates[0]
		if named, ok := tenant.Spec.Homes[contract]; ok {
			home = ""
			for _, c := range candidates {
				if c == named {
					home = named
				}
			}
			if home == "" {
				return nil
			}
		}
		return &homesync.HomeOf{Profile: home, URL: contractURL(providedEndpointOf(in.Profiles[home], contract), in.Namespace)}
	}
	plan.Home.Calendar = homeOf("calendar-store")
	plan.Home.Contacts = homeOf("contacts-store")

	for _, name := range installed {
		p := in.Profiles[name]
		if p == nil || p.Spec.HomeSync == nil {
			continue
		}
		hs := p.Spec.HomeSync
		app := homesync.App{Profile: name, URL: contractURL(&hs.Endpoint, in.Namespace), Kinds: map[homesync.Kind]homesync.Mapping{}}
		if hs.Calendar != nil {
			app.Kinds[homesync.Calendar] = mappingOf(hs.Calendar)
		}
		if hs.Contacts != nil {
			app.Kinds[homesync.Contacts] = mappingOf(hs.Contacts)
		}
		plan.Apps = append(plan.Apps, app)
	}
	return plan
}

func providedEndpointOf(p *gentianov1alpha1.ComponentProfile, contract string) *gentianov1alpha1.ContractEndpoint {
	for _, c := range p.Spec.Provides {
		if c.Name == contract && c.Endpoint != nil {
			return c.Endpoint
		}
	}
	return nil
}

func mappingOf(m *gentianov1alpha1.HomeSyncMapping) homesync.Mapping {
	out := homesync.Mapping{Driver: m.Driver, Model: m.Model, Fields: map[homesync.Field]string{}}
	for part, field := range m.Fields {
		out.Fields[homesync.Field(part)] = field
	}
	if m.Filter != nil {
		out.Filter = json.RawMessage(m.Filter.Raw)
	}
	if len(m.Defaults) > 0 {
		out.Defaults = map[string]json.RawMessage{}
		for k, v := range m.Defaults {
			out.Defaults[k] = json.RawMessage(v.Raw)
		}
	}
	return out
}

// ensureHomeSyncPlan writes the tenant's plan, or removes it where the
// tenant has no home sync.
func (r *TenantReconciler) ensureHomeSyncPlan(ctx context.Context, tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) error {
	plan := homeSyncPlan(tenant, in)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: HomeSyncPlanName, Namespace: in.Namespace}}
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
		cm.Data = map[string]string{HomeSyncPlanKey: string(raw)}
		return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("write the home sync's plan: %w", err)
	}
	return nil
}

// client0 is a delete's error, with a ConfigMap already gone counted as
// removed.
func client0(err error) error {
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
