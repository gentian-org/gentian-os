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
//   - a person of another tenant is given the positions the sign-in names;
//     where the sign-in names positions held at home (holding), only while
//     the person holds one of them in the source tenant, and those
//     positions are taken from a person who no longer does.
//
// Given at every arrival, which writes nothing when it is held already. The
// realm tells after the sign-in has happened, so the first page a person
// opens on their very first arrival may be refused; the next is not.
//
// Who was given the administrator's position this way is kept on the tenant
// (enteredFromKernelAnnotation), so that it is taken back when the tenant
// stops letting the cluster's administrators in.
//
// A person who makes an account on the realm's registration page is given
// the positions spec.signUp.positions names, once, when the realm says the
// account was made.

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

// notAdministrators is positions without the administrators' and without
// repeats: never given by an arrival, whatever a claim written past the
// schema says.
func notAdministrators(positions []string) []string {
	var out []string
	for _, p := range positions {
		if p != rights.PositionTenantAdministrator && p != rights.PositionAppAdministrator && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// arrivalPositions is what a person who came into tenant through provider
// is given whoever they are, and whether it is the cluster administrator's
// entry. What depends on what they hold at home is heldAtHome's.
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
		if from.Tenant != source || len(from.Holding) > 0 {
			continue
		}
		positions = append(positions, from.Positions...)
	}
	return notAdministrators(positions), false
}

// heldAtHome are the entries of the tenant's sign-in that give their
// positions only to who holds a position at home, for a person who came in
// through provider, with the source tenant they name.
func heldAtHome(tenant *gentianov1alpha1.Tenant, provider string) (source string, entries []gentianov1alpha1.TenantSignInFrom) {
	source, ok := strings.CutPrefix(provider, "tenant-")
	if !ok {
		return "", nil
	}
	for _, from := range tenant.Spec.SignInFrom {
		if from.Tenant == source && len(from.Holding) > 0 {
			entries = append(entries, from)
		}
	}
	return source, entries
}

// PersonEntered implements membership.Arrivals.
func (a *RealmArrivals) PersonEntered(ctx context.Context, realm, sub, provider, providerUser string) error {
	if a.Rights == nil {
		return nil
	}
	tenants, err := a.tenantsOfRealm(ctx, realm, true)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		positions, fromKernel := arrivalPositions(tenant, provider)
		give, take, err := a.byHolding(ctx, tenant, provider, providerUser)
		if err != nil {
			return err
		}
		if err := a.give(ctx, tenant, sub, notAdministrators(append(positions, give...)), "provider", provider); err != nil {
			return err
		}
		for _, position := range take {
			// Nor is it taken here: what administers is not an arrival's,
			// and the tenant's last administrator is never reached from it.
			if slices.Contains(positions, position) || slices.Contains(give, position) || tenant.PositionAdministers(position) {
				continue
			}
			_, err := rights.Take(ctx, a.Rights, tenant.Name, sub, rights.Assignment{Position: position})
			if err != nil {
				return fmt.Errorf("take %s in %s: %w", position, tenant.Name, err)
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

// byHolding is what the entries of the tenant's sign-in that ask what a
// person holds at home give the person who came in through provider, and
// what they take from them: an entry's positions are given while the person
// holds one of the positions it names in the source tenant, and taken once
// they do not. A person the realm did not say who they are at the provider
// is given nothing by such an entry, and nothing is taken.
func (a *RealmArrivals) byHolding(
	ctx context.Context, tenant *gentianov1alpha1.Tenant, provider, providerUser string,
) (give, take []string, err error) {
	source, entries := heldAtHome(tenant, provider)
	if len(entries) == 0 {
		return nil, nil, nil
	}
	if providerUser == "" {
		log.FromContext(ctx).Info("a person who arrived is given nothing for what they hold at home: the realm did not say who they are there",
			"tenant", tenant.Name, "provider", provider)
		return nil, nil, nil
	}
	for _, from := range entries {
		held, err := rights.HoldsAnyPosition(ctx, a.Rights, source, providerUser, from.Holding)
		if errors.Is(err, rights.ErrInvalidID) {
			// Not an account id the store can name: nobody there.
			log.FromContext(ctx).Info("a person who arrived is given nothing for what they hold at home: not an account id of the source",
				"tenant", tenant.Name, "provider", provider)
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read what a person holds in %s: %w", source, err)
		}
		if held {
			give = append(give, from.Positions...)
		} else {
			take = append(take, from.Positions...)
		}
	}
	return notAdministrators(give), notAdministrators(take), nil
}

// give gives sub the positions in tenant. A position the tenant does not
// have is said and passed over.
func (a *RealmArrivals) give(ctx context.Context, tenant *gentianov1alpha1.Tenant, sub string, positions []string, how ...any) error {
	for _, position := range positions {
		if tenant.PositionAdministers(position) {
			// Administering is given by an administrator, never on arrival.
			log.FromContext(ctx).Info("a person is not given a position that administers",
				append([]any{"tenant", tenant.Name, "position", position}, how...)...)
			continue
		}
		_, err := rights.Give(ctx, a.Rights, tenant.Name, sub, rights.Assignment{Position: position})
		if errors.Is(err, rights.ErrNoSuchThing) {
			// A position the tenant no longer declares, or a tenant whose
			// positions are not written yet: nothing to hold.
			log.FromContext(ctx).Info("a person is not given a position the tenant has not",
				append([]any{"tenant", tenant.Name, "position", position}, how...)...)
			continue
		}
		if err != nil {
			return fmt.Errorf("give %s in %s: %w", position, tenant.Name, err)
		}
	}
	return nil
}

// PersonRegistered implements membership.Registrations: an account a
// person made on the realm's registration page is given the positions the
// tenant names for it.
func (a *RealmArrivals) PersonRegistered(ctx context.Context, realm, sub string) error {
	if a.Rights == nil {
		return nil
	}
	tenants, err := a.tenantsOfRealm(ctx, realm, true)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if err := a.give(ctx, tenant, sub, notAdministrators(tenant.SignUpPositions()), "at", "sign-up"); err != nil {
			return err
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
