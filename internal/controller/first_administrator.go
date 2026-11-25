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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	rights "github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// A tenant's first administrator.
//
// The identity reconciler makes the account (makeAdminJob). What makes it an
// administrator is the position every tenant has for that, and somebody has
// to hold it before anybody in the tenant can give anything: the operator
// gives it to that one account, once.
//
// Once, because who administers a tenant is the tenant's to change from
// then on. The first administrator may hand the position to somebody else
// and lose it; an operator that gave it back at every reconcile would undo
// that. So the tenant is marked when it was given, with the account it was
// given to, and the step is not taken again -- not after a restore either,
// which brings back who held what.

const (
	// firstAdministratorAnnotation is on a tenant whose first administrator
	// was given the position; its value is that account's id.
	firstAdministratorAnnotation = "gentianos.io/first-administrator"
	// firstAdministratorRequeueAfter is how long the step waits for what it
	// needs: the account, and the tenant's positions in the store.
	firstAdministratorRequeueAfter = 15 * time.Second
)

// accountID answers the id of a realm's account by its username, or "".
type accountID func(ctx context.Context, realm, username string) (string, error)

// keycloakAccountID asks the identity provider with the operator's own
// credential.
func (r *TenantReconciler) keycloakAccountID(ctx context.Context, realm, username string) (string, error) {
	kc, err := authz.LoadKeycloakAdminClient(ctx, r.Client)
	if err != nil {
		return "", fmt.Errorf("load keycloak admin: %w", err)
	}
	return kc.UserIDByUsername(ctx, realm, username)
}

// ensureFirstAdministrator gives the tenant's first administrator the
// position Tenant administrator, once. A tenant that adopts the kernel
// realm has no account of its own made for it: the cluster's administrators
// administer it, through the tenant's consent.
func (r *TenantReconciler) ensureFirstAdministrator(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if r.Rights == nil || r.adoptsKernelRealm(tenant) {
		return ctrl.Result{}, nil
	}
	if tenant.Annotations[firstAdministratorAnnotation] != "" {
		return ctrl.Result{}, nil
	}
	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, conditionIdentityReady) {
		return ctrl.Result{RequeueAfter: firstAdministratorRequeueAfter}, nil
	}
	lookup := r.FirstAdministratorID
	if lookup == nil {
		lookup = r.keycloakAccountID
	}
	username := r.tenantAdminUsername(tenant)
	id, err := lookup(ctx, keycloak.RealmName(tenant), username)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("find the first administrator's account: %w", err)
	}
	if id == "" {
		// The Job that makes the account has not finished, or the account
		// was removed before it was ever an administrator. Asked again.
		return ctrl.Result{RequeueAfter: firstAdministratorRequeueAfter}, nil
	}
	_, err = rights.Give(ctx, r.Rights, tenant.Name, id, rights.Assignment{Position: rights.PositionTenantAdministrator})
	if errors.Is(err, rights.ErrNoSuchThing) {
		// The projection has not written the tenant's positions yet.
		return ctrl.Result{RequeueAfter: firstAdministratorRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("give the first administrator the position: %w", err)
	}
	orig := tenant.DeepCopy()
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[firstAdministratorAnnotation] = id
	if err := r.Patch(ctx, tenant, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark the first administrator as given: %w", err)
	}
	log.FromContext(ctx).Info("the tenant's first administrator holds the position",
		"tenant", tenant.Name, "position", rights.PositionTenantAdministrator, "account", id)
	return ctrl.Result{}, nil
}
