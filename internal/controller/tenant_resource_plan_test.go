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

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// The plan event is written from what landed: the annotations Argo CD synced
// from the director's commit, compared with what the status says was last
// recorded. These tests drive recordResourcePlan with a store that is a
// slice, and a catalogue of one plan with a SKU.

type recordedPlans struct {
	events []usage.PlanEvent
	last   *usage.PlanEvent
	fail   error
}

func (r *recordedPlans) EnsureSchema(context.Context) error { return r.fail }
func (r *recordedPlans) LastPlanBefore(context.Context, time.Time) (usage.PlanEvent, bool, error) {
	if r.last == nil {
		return usage.PlanEvent{}, false, nil
	}
	return *r.last, true, nil
}
func (r *recordedPlans) RecordPlanEvent(_ context.Context, e usage.PlanEvent) error {
	r.events = append(r.events, e)
	return nil
}

func planReconciler(t *testing.T, store *recordedPlans) *TenantReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	nodes4 := &gentianov1alpha1.ResourcePlan{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes-4"},
		Spec:       gentianov1alpha1.ResourcePlanSpec{DisplayName: "Four nodes", Tier: 2, ProductSku: "GTN-N4"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodes4).Build()
	return &TenantReconciler{
		Client: c,
		PlanEventStore: func(context.Context, string) (planEventStore, error) {
			if store == nil {
				return nil, errors.New("no database yet")
			}
			return store, nil
		},
	}
}

func tenantOn(plan, actor, recorded string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo", Annotations: map[string]string{}}}
	if plan != "" {
		t.Annotations[gentianov1alpha1.ResourcePlanAnnotation] = plan
	}
	if actor != "" {
		t.Annotations[gentianov1alpha1.ResourcePlanSetByAnnotation] = actor
	}
	t.Status.ResourcePlan = recorded
	return t
}

func TestALandedPlanChangeIsRecordedWithItsActorAndSKU(t *testing.T) {
	store := &recordedPlans{}
	r := planReconciler(t, store)
	tenant := tenantOn("nodes-4", "tom@example.com", "nodes-2")

	r.recordResourcePlan(context.Background(), tenant)

	if len(store.events) != 1 {
		t.Fatalf("events = %+v", store.events)
	}
	e := store.events[0]
	if e.FromPlan != "nodes-2" || e.ToPlan != "nodes-4" || e.Actor != "tom@example.com" || e.ProductSku != "GTN-N4" {
		t.Fatalf("event = %+v", e)
	}
	if tenant.Status.ResourcePlan != "nodes-4" {
		t.Fatalf("status = %q; the next reconcile would record it again", tenant.Status.ResourcePlan)
	}
}

func TestNothingIsRecordedWhenTheStatusAlreadySaysSo(t *testing.T) {
	store := &recordedPlans{}
	r := planReconciler(t, store)
	for _, tenant := range []*gentianov1alpha1.Tenant{
		tenantOn("nodes-4", "tom@example.com", "nodes-4"), // recorded already
		tenantOn("", "", ""),                              // never put on a plan through git
	} {
		r.recordResourcePlan(context.Background(), tenant)
	}
	if len(store.events) != 0 {
		t.Fatalf("events = %+v", store.events)
	}
}

// A tenant whose history predates the status field: the previous plan is
// what the store last saw, and a history that already ends on the annotated
// plan is not a move at all -- the status is caught up and nothing written.
func TestTheStoresLastEventStandsInForAMissingStatus(t *testing.T) {
	store := &recordedPlans{last: &usage.PlanEvent{ToPlan: "nodes-2"}}
	r := planReconciler(t, store)
	tenant := tenantOn("nodes-4", "alice@example.com", "")
	r.recordResourcePlan(context.Background(), tenant)
	if len(store.events) != 1 || store.events[0].FromPlan != "nodes-2" {
		t.Fatalf("events = %+v", store.events)
	}

	store = &recordedPlans{last: &usage.PlanEvent{ToPlan: "nodes-4"}}
	r = planReconciler(t, store)
	tenant = tenantOn("nodes-4", "alice@example.com", "")
	r.recordResourcePlan(context.Background(), tenant)
	if len(store.events) != 0 || tenant.Status.ResourcePlan != "nodes-4" {
		t.Fatalf("events = %+v, status = %q", store.events, tenant.Status.ResourcePlan)
	}
}

// A store that is not there yet leaves the status alone, so the change is
// recorded by a later reconcile rather than lost; a plan the catalogue does
// not know is recorded without a SKU rather than not at all.
func TestAnUnavailableStoreDefersAndAnUnknownPlanStillCounts(t *testing.T) {
	r := planReconciler(t, nil)
	tenant := tenantOn("nodes-4", "tom@example.com", "nodes-2")
	r.recordResourcePlan(context.Background(), tenant)
	if tenant.Status.ResourcePlan != "nodes-2" {
		t.Fatalf("status advanced to %q with nothing recorded", tenant.Status.ResourcePlan)
	}

	store := &recordedPlans{}
	r = planReconciler(t, store)
	tenant = tenantOn("hand-made", "", "nodes-2")
	r.recordResourcePlan(context.Background(), tenant)
	if len(store.events) != 1 || store.events[0].ToPlan != "hand-made" || store.events[0].ProductSku != "" {
		t.Fatalf("events = %+v", store.events)
	}
}
