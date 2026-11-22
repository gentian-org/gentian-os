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

	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	dauthz "github.com/gentian-org/gentian-os/internal/director/authz"
)

// SecondFactorAttribute is the account attribute the realm's sign-in flow
// reads: an account that carries it as "true" gives a second factor at every
// sign-in, and enrolls first if it has none. The realm's user profile
// declares it for administrators alone to see and change, so a person cannot
// take it off their own account.
const SecondFactorAttribute = "gentianRequiresMFA"

// secondFactorAccounts is the realm's side of the rule, as far as the rule
// needs it.
type secondFactorAccounts interface {
	ListRealmAccounts(ctx context.Context, realm string) ([]authz.KeycloakUser, error)
	SetUserAttribute(ctx context.Context, realm, userID, name, value string) error
}

// ensureSecondFactorMarks holds the realm's accounts to the rule that who
// administers gives a second factor: every account that administers a
// tenant of the realm carries the mark, and no other account does.
//
// Who administers is the rights store's to say, and the realm cannot ask
// it in the middle of a sign-in flow; so the platform writes what the store
// says onto the account. A change of what a person holds asks for this pass
// at once (RightsChanged); a change of what a position holds is found by
// the next one.
//
// A realm several tenants share is read for all of them: the mark stays
// while the person administers any.
func (r *TenantReconciler) ensureSecondFactorMarks(ctx context.Context, tenant *gentianov1alpha1.Tenant, kc secondFactorAccounts) error {
	if r.Rights == nil {
		return nil
	}
	realm := keycloakRealmName(tenant)
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return err
	}
	administers := map[string]bool{}
	for i := range tenants.Items {
		other := &tenants.Items[i]
		if keycloakRealmName(other) != realm || other.DeletionTimestamp != nil {
			continue
		}
		subjects, err := dauthz.Administrators(ctx, r.Rights, other.Name)
		if err != nil {
			return fmt.Errorf("read the administrators of %s: %w", other.Name, err)
		}
		for _, s := range subjects {
			administers[s] = true
		}
	}
	accounts, err := kc.ListRealmAccounts(ctx, realm)
	if err != nil {
		return fmt.Errorf("list the accounts of realm %s: %w", realm, err)
	}
	for _, a := range secondFactorChanges(accounts, administers) {
		value := ""
		if administers[a.ID] {
			value = "true"
		}
		if err := kc.SetUserAttribute(ctx, realm, a.ID, SecondFactorAttribute, value); err != nil {
			return fmt.Errorf("mark account %s: %w", a.ID, err)
		}
		log.FromContext(ctx).Info("second factor rule set on an account",
			"realm", realm, "account", a.ID, "required", value == "true")
	}
	return nil
}

// secondFactorChanges is the accounts whose mark differs from what the
// store says: an administrator without it, anybody else with it.
func secondFactorChanges(accounts []authz.KeycloakUser, administers map[string]bool) []authz.KeycloakUser {
	var out []authz.KeycloakUser
	for _, a := range accounts {
		marked := false
		for _, v := range a.Attributes[SecondFactorAttribute] {
			if v == "true" {
				marked = true
			}
		}
		if marked != administers[a.ID] {
			out = append(out, a)
		}
	}
	return out
}
