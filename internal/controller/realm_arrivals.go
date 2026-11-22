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
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	rights "github.com/gentian-org/gentian-os/internal/director/authz"
)

// People who come into a tenant's realm from another realm.
//
// A tenant lets two kinds of people in who are accounts elsewhere: the
// cluster's administrators, through the identity provider "kernel"
// (spec.platformAccess.adminsEnter), and the people of another tenant,
// through "tenant-<source>" (spec.signInFrom). The realm decides who gets
// through the provider; what such a person holds in the tenant is the rights
// store's, and the operator gives it when the realm says they arrived:
//
//   - a cluster administrator is given the position Tenant administrator;
//   - a person of another tenant is given the positions the sign-in names.
//
// Given at every arrival, which writes nothing when it is held already. The
// realm tells after the sign-in has happened, so the first page a person
// opens on their very first arrival may be refused; the next is not.
//
// Who was given the administrator's position this way is kept on the tenant
// (enteredFromKernelAnnotation), so that it is taken back when the tenant
// stops letting the cluster's administrators in.

const (
	// kernelProvider is the identity provider a tenant's realm lets the
	// cluster's administrators in through.
	kernelProvider = "kernel"
	// enteredFromKernelAnnotation lists the accounts given the position
	// Tenant administrator because they came in through the kernel's
	// provider: ids, comma-separated, sorted.
	enteredFromKernelAnnotation = "gentianos.io/entered-from-kernel"
)

// RealmArrivals gives people who arrive from another realm what the tenant
// declares for them, and takes what a removed person held.
type RealmArrivals struct {
	Client client.Client
	Rights rights.Store
}

// tenantsOfRealm are the tenants with that realm to themselves: the ones a
// realm's identity providers belong to.
func (a *RealmArrivals) tenantsOfRealm(ctx context.Context, realm string, own bool) ([]*gentianov1alpha1.Tenant, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := a.Client.List(ctx, list); err != nil {
		return nil, err
	}
	var out []*gentianov1alpha1.Tenant
	for i := range list.Items {
		t := &list.Items[i]
		if t.DeletionTimestamp != nil || keycloakRealmName(t) != realm {
			continue
		}
		if own && t.Name != realm {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// arrivalPositions is what a person who came into tenant through provider
// is given, and whether it is the cluster administrator's entry.
func arrivalPositions(tenant *gentianov1alpha1.Tenant, provider string) (positions []string, fromKernel bool) {
	if provider == kernelProvider {
		if tenant.PlatformAdminsEnter() {
			return []string{rights.PositionTenantAdministrator}, true
		}
		return nil, false
	}
	source, ok := strings.CutPrefix(provider, "tenant-")
	if !ok {
		return nil, false
	}
	for _, from := range tenant.Spec.SignInFrom {
		if from.Tenant != source || !slices.Contains(tenantNamesSigningInTo(tenant), source) {
			continue
		}
		for _, p := range from.Positions {
			// Never an administrator's, whatever a claim written past the
			// schema says.
			if p != rights.PositionTenantAdministrator && p != rights.PositionAppAdministrator && !slices.Contains(positions, p) {
				positions = append(positions, p)
			}
		}
	}
	return positions, false
}

// tenantNamesSigningInTo are the sources a tenant names; the other half of
// the consent, the source naming this tenant, is what made the realm's
// identity provider, without which nobody arrives through it.
func tenantNamesSigningInTo(tenant *gentianov1alpha1.Tenant) []string {
	out := make([]string, 0, len(tenant.Spec.SignInFrom))
	for _, f := range tenant.Spec.SignInFrom {
		out = append(out, f.Tenant)
	}
	return out
}

// PersonEntered implements membership.Arrivals.
func (a *RealmArrivals) PersonEntered(ctx context.Context, realm, sub, provider string) error {
	if a.Rights == nil {
		return nil
	}
	tenants, err := a.tenantsOfRealm(ctx, realm, true)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		positions, fromKernel := arrivalPositions(tenant, provider)
		for _, position := range positions {
			_, err := rights.Give(ctx, a.Rights, tenant.Name, sub, rights.Assignment{Position: position})
			if errors.Is(err, rights.ErrNoSuchThing) {
				// A position the tenant no longer declares, or a tenant
				// whose positions are not written yet: nothing to hold.
				log.FromContext(ctx).Info("a person who arrived is not given a position the tenant has not",
					"tenant", tenant.Name, "position", position, "provider", provider)
				continue
			}
			if err != nil {
				return fmt.Errorf("give %s in %s: %w", position, tenant.Name, err)
			}
		}
		if fromKernel {
			if err := a.remember(ctx, tenant, sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// remember notes on the tenant that sub holds the administrator's position
// because they came in from the kernel realm.
func (a *RealmArrivals) remember(ctx context.Context, tenant *gentianov1alpha1.Tenant, sub string) error {
	entered := enteredFromKernel(tenant)
	if slices.Contains(entered, sub) {
		return nil
	}
	entered = append(entered, sub)
	sort.Strings(entered)
	orig := tenant.DeepCopy()
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[enteredFromKernelAnnotation] = strings.Join(entered, ",")
	return a.Client.Patch(ctx, tenant, client.MergeFrom(orig))
}

func enteredFromKernel(tenant *gentianov1alpha1.Tenant) []string {
	var out []string
	for _, id := range strings.Split(tenant.Annotations[enteredFromKernelAnnotation], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// PersonRemoved implements membership.Departures: what a removed person
// held in the tenants of the realm goes with them. A tenant's last
// administrator is left as the store has it: the tenant must not end up
// with nobody, and the entry names an account that can no longer sign in.
func (a *RealmArrivals) PersonRemoved(ctx context.Context, realm, sub string) error {
	if a.Rights == nil {
		return nil
	}
	tenants, err := a.tenantsOfRealm(ctx, realm, false)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		_, err := rights.RemovePerson(ctx, a.Rights, tenant.Name, sub)
		if errors.Is(err, rights.ErrLastAdministrator) {
			log.FromContext(ctx).Info("a removed person was the tenant's last administrator; the entry stays",
				"tenant", tenant.Name, "account", sub)
			continue
		}
		if err != nil && !errors.Is(err, rights.ErrInvalidID) {
			return fmt.Errorf("remove what a removed person held in %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// ensureKernelEntryWithdrawn takes the administrator's position back from
// the accounts that were given it for coming in from the kernel realm, once
// the tenant no longer lets the cluster's administrators in.
func (r *TenantReconciler) ensureKernelEntryWithdrawn(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	entered := enteredFromKernel(tenant)
	if r.Rights == nil || tenant.PlatformAdminsEnter() || len(entered) == 0 {
		return nil
	}
	for _, sub := range entered {
		_, err := rights.Take(ctx, r.Rights, tenant.Name, sub, rights.Assignment{Position: rights.PositionTenantAdministrator})
		if errors.Is(err, rights.ErrLastAdministrator) {
			// The tenant has nobody else: left, and said.
			log.FromContext(ctx).Info("the position is not taken back from the tenant's last administrator",
				"tenant", tenant.Name, "account", sub)
			continue
		}
		if err != nil && !errors.Is(err, rights.ErrNoSuchThing) {
			return fmt.Errorf("take the position back from %s: %w", sub, err)
		}
	}
	orig := tenant.DeepCopy()
	delete(tenant.Annotations, enteredFromKernelAnnotation)
	return r.Patch(ctx, tenant, client.MergeFrom(orig))
}
