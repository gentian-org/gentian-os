/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestTupleKeysNotIn(t *testing.T) {
	t.Parallel()
	prev := []TupleKey{
		{User: "user:a", Relation: "member", Object: "tenant:demo"},
		{User: "user:b", Relation: "member", Object: "tenant:demo"},
	}
	next := []TupleKey{
		{User: "user:b", Relation: "member", Object: "tenant:demo"},
		{User: "user:c", Relation: "member", Object: "tenant:demo"},
	}
	deletes := TupleKeysNotIn(prev, next)
	if len(deletes) != 1 || deletes[0].User != "user:a" {
		t.Fatalf("deletes = %#v", deletes)
	}
	writes := TupleKeysNotIn(next, prev)
	if len(writes) != 1 || writes[0].User != "user:c" {
		t.Fatalf("writes = %#v", writes)
	}
}

func TestGrantTupleSyncPlan(t *testing.T) {
	t.Parallel()
	grant := &gentianov1alpha1.AppGrant{
		Spec: gentianov1alpha1.AppGrantSpec{
			App: "provider",
			Consume: []gentianov1alpha1.ConsumeGrantSpec{
				{Contract: "files", Granted: []string{"read"}},
			},
		},
	}
	prev := GrantTupleKeys("demo", grant)
	grant.Spec.Consume[0].Granted = []string{"read", "write"}
	writes, deletes, next := grantTupleSyncPlanForTest("demo", grant, prev)
	if len(deletes) != 0 {
		t.Fatalf("expected no deletes, got %#v", deletes)
	}
	if len(writes) == 0 {
		t.Fatal("expected write tuples for new capability")
	}
	if len(next) <= len(prev) {
		t.Fatalf("next keys = %d, prev = %d", len(next), len(prev))
	}
}

func grantTupleSyncPlanForTest(tenant string, grant *gentianov1alpha1.AppGrant, prev []TupleKey) (writes []Tuple, deletes []TupleKey, next []TupleKey) {
	desired := GrantTuples(tenant, grant)
	desiredKeys := TupleKeysFromTuples(desired)
	deletes = TupleKeysNotIn(prev, desiredKeys)
	writeKeys := TupleKeysNotIn(desiredKeys, prev)
	writes = TuplesMatchingKeys(desired, writeKeys)
	return writes, deletes, desiredKeys
}
