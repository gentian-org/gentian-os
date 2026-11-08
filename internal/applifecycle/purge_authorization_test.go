/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/backup"
)

type purgedStores struct{ purged []string }

func (p *purgedStores) Purge(_ context.Context, tenant, component string) error {
	p.purged = append(p.purged, tenant+"/"+component)
	return nil
}

// An app the arbiter recorded a store for has the store among what it
// holds, and a purge deletes it through the arbiter, in the teardown order.
func TestAPurgeDeletesTheAppsAuthorizationStore(t *testing.T) {
	ctx := context.Background()
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: arbiter.StoresConfigMap, Namespace: arbiter.Namespace},
		Data:       map[string]string{"demo.ledger": `{"store":"s1"}`, "other.ledger": `{"store":"s2"}`},
	}
	c := fake.NewClientBuilder().WithObjects(record).Build()
	stores := &purgedStores{}
	s := &Service{client: c, authorization: stores}
	held, err := s.authorizationStores(ctx, "demo")
	if err != nil || len(held) != 1 || !held["ledger"] {
		t.Fatalf("held = %v, %v", held, err)
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	steps := s.purgeSteps(tenant, &gentianov1alpha1.ComponentProfile{}, "ledger", backup.Provisioned{}, true)
	found := false
	for _, step := range steps {
		if step.kind == KindAuthorization {
			found = true
			if err := step.run(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found || len(stores.purged) != 1 || stores.purged[0] != "demo/ledger" {
		t.Fatalf("steps found=%v purged=%v", found, stores.purged)
	}
	if steps := s.purgeSteps(tenant, &gentianov1alpha1.ComponentProfile{}, "wiki", backup.Provisioned{}, false); len(steps) > 0 {
		for _, step := range steps {
			if step.kind == KindAuthorization {
				t.Fatal("an app without a store is purged of one")
			}
		}
	}
}
