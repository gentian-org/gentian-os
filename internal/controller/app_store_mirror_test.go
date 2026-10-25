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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
)

type platformCalls struct{ memberships []arbiter.MembershipsRequest }

func (p *platformCalls) SetMemberships(_ context.Context, req arbiter.MembershipsRequest) error {
	p.memberships = append(p.memberships, req)
	return nil
}

func (p *platformCalls) GroupDeleted(context.Context, arbiter.GroupDeletedRequest) error { return nil }

// A realm's memberships go to the arbiter for the one tenant whose realm it
// is, and nowhere while the cluster runs no app authorization or when the
// realm is nobody's or several tenants'.
func TestARealmsMembershipsReachTheArbiterForItsTenantAlone(t *testing.T) {
	ctx := context.Background()
	acme := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	platform := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}}}
	twinA := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "twin-a"},
		Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "shared"}}}
	twinB := twinA.DeepCopy()
	twinB.Name = "twin-b"
	set := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"}}
	c := fake.NewClientBuilder().WithScheme(rightsScheme(t)).WithObjects(acme, platform, twinA, twinB, set).Build()
	calls := &platformCalls{}
	m := &AppStoreMirror{Client: c, Namespace: "kernel-control", Arbiter: calls}

	if err := m.SetUserGroups(ctx, "acme", "u1", []string{"staff"}); err != nil || len(calls.memberships) != 0 {
		t.Fatalf("without the engine: %v %v", err, calls.memberships)
	}
	set.Data = map[string]string{EngineAuthorization: engineWanted}
	if err := c.Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	for _, realm := range []string{"acme", "kernel", "shared", "nobody"} {
		if err := m.SetUserGroups(ctx, realm, "u1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls.memberships) != 2 || calls.memberships[0].Tenant != "acme" || calls.memberships[1].Tenant != "platform" ||
		calls.memberships[1].Realm != "kernel" || calls.memberships[0].Groups == nil {
		t.Fatalf("told the arbiter %+v", calls.memberships)
	}
}
