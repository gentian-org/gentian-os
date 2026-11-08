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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// The record lands in every namespace a reader runs in, the same in each, and
// is what the operator finds again after a restart.
func TestTheAuthorizationStoreIsRecordedWhereItsReadersRun(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).Build()
	if got := RecordedAuthorizationStore(ctx, c, "kernel-control"); got != "" {
		t.Fatalf("recorded before anything was written: %q", got)
	}
	namespaces := []string{"kernel-control", "kernel-edge"}
	if err := RecordAuthorizationStore(ctx, c, namespaces, "s1", "m1"); err != nil {
		t.Fatal(err)
	}
	// A new model is the same record with a new model id.
	if err := RecordAuthorizationStore(ctx, c, namespaces, "s1", "m2"); err != nil {
		t.Fatal(err)
	}
	for _, ns := range namespaces {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: authz.PinConfigMap, Namespace: ns}, cm); err != nil {
			t.Fatal(err)
		}
		if cm.Data[authz.PinStoreKey] != "s1" || cm.Data[authz.PinModelKey] != "m2" {
			t.Fatalf("%s: %v", ns, cm.Data)
		}
	}
	if got := RecordedAuthorizationStore(ctx, c, "kernel-control"); got != "s1" {
		t.Fatalf("recorded store = %q, want s1", got)
	}
}
