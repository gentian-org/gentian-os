/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package placement decides which placeable components the platform puts
// in which tenants.
//
// A placeable profile (ComponentProfile spec.placement) names the tenants it
// goes to and, optionally, a condition it lasts for. The decision is made
// here, from what the cluster says; the director's placement worker writes
// it into each tenant's manifest as entries of spec.apps, and the operator
// runs those entries as it runs any app.
//
// A profile placed while the cluster serves models is not decided here: the
// worker finds it in the catalog's index before it is on the cluster.
package placement

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// RequiresUsageStatisticsAnnotation on a ComponentProfile says the component
// is placed only on a cluster that reports usage statistics. An annotation
// and not a field, so a catalog can say it to a cluster of any version.
const RequiresUsageStatisticsAnnotation = "gentianos.io/requires-usage-statistics"

// Set is the placement for one kind of tenant.
type Set struct {
	// Wanted are the profiles the tenant has.
	Wanted []string `json:"wanted"`
	// Undecided are the profiles whose placement could not be decided: a
	// fact it depends on could not be read. What is placed of them stays,
	// and nothing of them is placed.
	Undecided []string `json:"undecided"`
}

// Has reports whether the set places a profile.
func (s Set) Has(name string) bool { return contains(s.Wanted, name) }

// Decided reports whether the set decided a profile either way.
func (s Set) Decided(name string) bool { return !contains(s.Undecided, name) }

// Answer is the placement for a user tenant and for the platform tenant,
// and the contracts between placed components the platform pre-approves.
type Answer struct {
	Users    Set `json:"users"`
	Platform Set `json:"platform"`
	// Grants are granted in every tenant that has both components placed.
	Grants []Grant `json:"grants"`
}

// Grant is one contract the platform pre-approves between two components
// it places: App may consume Contract from Provider, with Capabilities.
//
// The platform's own components are written to work together; asking each
// tenant's administrator to approve that is a step that can only be got
// wrong. So a contract is pre-approved when both sides are placed by the
// platform itself (ByOperator), both are of platform trust, the consumer
// names the provider (spec.integrations[].provider) and the capabilities it
// asks for, and the provider provides the contract. Which profiles those are
// comes with the platform's own versions: its chart and its cluster
// catalogs. Anything else is granted by an administrator, as before.
type Grant struct {
	App          string   `json:"app"`
	Contract     string   `json:"contract"`
	Provider     string   `json:"provider"`
	Capabilities []string `json:"capabilities"`
}

// For is the placement for one tenant.
func (a Answer) For(platform bool) Set {
	if platform {
		return a.Platform
	}
	return a.Users
}

// Facts are what the decision depends on.
type Facts struct {
	// Profiles are the cluster's ComponentProfiles.
	Profiles []gentianov1alpha1.ComponentProfile
	// UsageStatistics is whether the cluster reports usage statistics.
	UsageStatistics bool
	// StoreOffered is whether the cluster offers an App Store; nil when
	// that could not be read.
	StoreOffered *bool
	// Gone are the profiles whose catalog entry has left the cluster's
	// catalog directory: still objects, placed no more.
	Gone map[string]bool
}

// Decide is the placement the facts call for.
func Decide(f Facts) Answer {
	var out Answer
	for i := range f.Profiles {
		profile := &f.Profiles[i]
		p := profile.PlacementOf()
		if p == nil || p.While == gentianov1alpha1.PlacementWhileLLM || !isApp(profile) {
			continue
		}
		if profile.Annotations[RequiresUsageStatisticsAnnotation] == "true" && !f.UsageStatistics {
			continue
		}
		if f.Gone[profile.Name] {
			continue
		}
		if p.While == gentianov1alpha1.PlacementWhileStoreOffered {
			if f.StoreOffered == nil {
				out.Users.Undecided = append(out.Users.Undecided, profile.Name)
				out.Platform.Undecided = append(out.Platform.Undecided, profile.Name)
				continue
			}
			if !*f.StoreOffered {
				continue
			}
		}
		switch p.Tenants {
		case gentianov1alpha1.PlacementEvery:
			out.Users.Wanted = append(out.Users.Wanted, profile.Name)
			out.Platform.Wanted = append(out.Platform.Wanted, profile.Name)
		case gentianov1alpha1.PlacementPlatform:
			out.Platform.Wanted = append(out.Platform.Wanted, profile.Name)
		case gentianov1alpha1.PlacementUsers:
			out.Users.Wanted = append(out.Users.Wanted, profile.Name)
		}
	}
	out.Grants = preApproved(f.Profiles, f.Gone)
	for _, s := range []*Set{&out.Users, &out.Platform} {
		sort.Strings(s.Wanted)
		sort.Strings(s.Undecided)
		if s.Wanted == nil {
			s.Wanted = []string{}
		}
		if s.Undecided == nil {
			s.Undecided = []string{}
		}
	}
	return out
}

// isApp reports whether a profile is certified for class app, the only
// class a tenant has an instance of.
func isApp(profile *gentianov1alpha1.ComponentProfile) bool {
	for _, c := range profile.Spec.Classes {
		if c == gentianov1alpha1.ComponentClassApp {
			return true
		}
	}
	return false
}

func contains(list []string, name string) bool {
	for _, have := range list {
		if have == name {
			return true
		}
	}
	return false
}

// ByOperator reports whether a profile is one the platform places for the
// life of a tenant or while a store is offered. Before placement was written
// into spec.apps such a component had no entry there, and the readers of
// spec.apps that serve installed apps -- stores, groups, the authorization
// store, exports, quotas, reports -- still leave it out (Installed). One
// placed while the cluster serves models always had its entry, and is read
// like any app.
func ByOperator(profile *gentianov1alpha1.ComponentProfile) bool {
	p := profile.PlacementOf()
	return p != nil && p.While != gentianov1alpha1.PlacementWhileLLM
}

// Installed are a tenant's entries of spec.apps without the components the
// platform places itself (ByOperator): what a tenant's administrator
// installed, and what the cluster's capabilities brought. An entry whose
// profile cannot be read is kept, as a reader of spec.apps always kept it.
func Installed(ctx context.Context, c client.Reader, apps []gentianov1alpha1.TenantApp) ([]gentianov1alpha1.TenantApp, error) {
	out := make([]gentianov1alpha1.TenantApp, 0, len(apps))
	for _, app := range apps {
		if !app.Placed {
			out = append(out, app)
			continue
		}
		name := app.Profile
		if name == "" && app.ProfileRef != nil {
			name = app.ProfileRef.Name
		}
		profile := &gentianov1alpha1.ComponentProfile{}
		err := c.Get(ctx, types.NamespacedName{Name: name}, profile)
		if apierrors.IsNotFound(err) {
			out = append(out, app)
			continue
		}
		if err != nil {
			return nil, err
		}
		if !ByOperator(profile) {
			out = append(out, app)
		}
	}
	return out, nil
}

// preApproved are the contracts between components the platform places
// that it pre-approves (Grant).
func preApproved(profiles []gentianov1alpha1.ComponentProfile, gone map[string]bool) []Grant {
	ours := map[string]*gentianov1alpha1.ComponentProfile{}
	for i := range profiles {
		p := &profiles[i]
		if ByOperator(p) && isApp(p) && !gone[p.Name] && p.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform {
			ours[p.Name] = p
		}
	}
	out := []Grant{}
	for _, consumer := range ours {
		for _, in := range consumer.Spec.Integrations {
			provider, ok := ours[in.Provider]
			if !ok || len(in.Capabilities) == 0 || provider.Name == consumer.Name {
				continue
			}
			for _, c := range provider.Spec.Provides {
				if c.Name == in.Contract {
					out = append(out, Grant{App: consumer.Name, Contract: in.Contract, Provider: provider.Name,
						Capabilities: append([]string{}, in.Capabilities...)})
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Contract < out[j].Contract
	})
	return out
}
