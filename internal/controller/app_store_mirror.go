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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
)

// AppStoreMirror carries a realm's memberships on to the app stores that
// mirror its groups (requires.services.authorization.groups: mirror), through
// the arbiter's platform path: the statements the membership listener has
// verified and applied to the kernel's store, every group of the realm, and
// to the stores of that realm's tenant alone.
//
// A realm is a tenant's when exactly one tenant has it. One that no tenant
// has, or that several share, reaches no store: which tenant's components a
// person's groups are told to is not a guess.
type AppStoreMirror struct {
	Client client.Reader
	// Namespace is the control namespace, where the engine set is.
	Namespace string
	Arbiter   interface {
		SetMemberships(ctx context.Context, req arbiter.MembershipsRequest) error
		GroupDeleted(ctx context.Context, req arbiter.GroupDeletedRequest) error
	}
}

// tenantOfRealm is the tenant whose realm this is, "" when there is not
// exactly one, or when the cluster runs no app authorization: then there is
// no store to carry anything to.
func (m *AppStoreMirror) tenantOfRealm(ctx context.Context, realm string) (string, error) {
	set := &corev1.ConfigMap{}
	if err := m.Client.Get(ctx, types.NamespacedName{Name: systemEnginesConfigMap, Namespace: m.Namespace}, set); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if set.Data[EngineAuthorization] != engineWanted {
		return "", nil
	}
	list := &gentianov1alpha1.TenantList{}
	if err := m.Client.List(ctx, list); err != nil {
		return "", err
	}
	var found []string
	for i := range list.Items {
		if tenantRealmName(&list.Items[i]) == realm {
			found = append(found, list.Items[i].Name)
		}
	}
	if len(found) != 1 {
		if len(found) > 1 {
			logf.FromContext(ctx).Info("a realm several tenants share reaches no app store", "realm", realm, "tenants", found)
		}
		return "", nil
	}
	return found[0], nil
}

func (m *AppStoreMirror) SetUserGroups(ctx context.Context, realm, sub string, groups []string) error {
	tenant, err := m.tenantOfRealm(ctx, realm)
	if err != nil || tenant == "" {
		return err
	}
	if groups == nil {
		groups = []string{}
	}
	if err := m.Arbiter.SetMemberships(ctx, arbiter.MembershipsRequest{Tenant: tenant, Realm: realm, Subject: sub, Groups: groups}); err != nil {
		return fmt.Errorf("mirror the memberships into %s's app stores: %w", tenant, err)
	}
	return nil
}

func (m *AppStoreMirror) RemoveGroup(ctx context.Context, realm, group string) error {
	tenant, err := m.tenantOfRealm(ctx, realm)
	if err != nil || tenant == "" {
		return err
	}
	if err := m.Arbiter.GroupDeleted(ctx, arbiter.GroupDeletedRequest{Tenant: tenant, Realm: realm, Group: group}); err != nil {
		return fmt.Errorf("mirror the deletion of a group into %s's app stores: %w", tenant, err)
	}
	return nil
}
