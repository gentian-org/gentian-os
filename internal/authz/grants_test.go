/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
