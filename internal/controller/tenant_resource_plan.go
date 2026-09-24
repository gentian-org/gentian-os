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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/resourceplan"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// The plan a tenant is on is chosen through the director and arrives here as
// two annotations on the Tenant, synced by Argo CD from the tenant's
// resource-plan.yaml: which plan, and who chose it. The quotas in the same
// patch are what the reconciler enforces. The usage history is the billing
// record, and what it wants is a plan EVENT -- when the tenant moved, from
// what, to what, by whom -- so the operator writes that event from what
// landed. A record written at request time would describe an intent that may
// never have synced; this one describes the ceiling the cluster went on to
// enforce, which is the one a bill can be defended with.
//
// Status.ResourcePlan is the plan whose event was last written. It is
// compared with the annotation on every reconcile and the store is opened
// only when they differ, which happens a handful of times in a tenant's life.

// planEventStore is the part of the usage store this file writes to.
type planEventStore interface {
	EnsureSchema(ctx context.Context) error
	LastPlanBefore(ctx context.Context, t time.Time) (usage.PlanEvent, bool, error)
	RecordPlanEvent(ctx context.Context, event usage.PlanEvent) error
}

// planEventStoreFor opens the usage store of one tenant. A field so that a
// test can hand the reconciler a store that is not a database.
type planEventStoreFor func(ctx context.Context, tenant string) (planEventStore, error)

// resourcePlanChange is the event a tenant's annotations describe when they
// name a plan the status has not recorded yet. ok is false when the two
// agree, or when the tenant has never been put on a plan through git.
//
// The previous plan is what the status recorded, or, for a tenant whose
// history predates the status field, the plan the store last saw; the caller
// fills that in. The SKU comes from the catalogue when the plan is in it. A
// plan the catalogue does not know is recorded all the same: it is what
// landed, and a bill that omits it would be a bill that omits a change.
func resourcePlanChange(
	tenant *gentianov1alpha1.Tenant, catalogue *resourceplan.Catalogue, previous string, now time.Time,
) (usage.PlanEvent, bool) {
	chosen := tenant.Annotations[gentianov1alpha1.ResourcePlanAnnotation]
	if chosen == "" || chosen == tenant.Status.ResourcePlan {
		return usage.PlanEvent{}, false
	}
	event := usage.PlanEvent{
		OccurredAt: now,
		FromPlan:   previous,
		ToPlan:     chosen,
		Actor:      tenant.Annotations[gentianov1alpha1.ResourcePlanSetByAnnotation],
	}
	if catalogue != nil {
		if plan := catalogue.Get(chosen); plan != nil {
			event.ProductSku = plan.Spec.ProductSku
		}
	}
	return event, true
}

// recordResourcePlan writes the plan event a landed change calls for and
// notes it on the status, which the caller then updates.
//
// A store that cannot be reached leaves the status as it was, so the next
// reconcile tries again; a tenant whose database is still being provisioned
// gets its event once the database exists rather than never. Nothing here
// returns an error: the plan is enforced by the quotas whatever happens to
// the record, and a billing gap must not hold the tenant's readiness.
func (r *TenantReconciler) recordResourcePlan(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	chosen := tenant.Annotations[gentianov1alpha1.ResourcePlanAnnotation]
	if chosen == "" || chosen == tenant.Status.ResourcePlan {
		return
	}
	logger := log.FromContext(ctx).WithName("resource-plan").WithValues("tenant", tenant.Name, "plan", chosen)

	catalogue, err := resourceplan.Load(ctx, r.Client)
	if err != nil {
		// The catalogue only supplies the SKU. Record without it rather than
		// lose the event to a transient list failure.
		logger.Info("resource plans unavailable; recording the plan change without its SKU", "reason", err.Error())
		catalogue = nil
	}

	open := r.PlanEventStore
	if open == nil {
		open = func(ctx context.Context, name string) (planEventStore, error) {
			return usage.StoreForTenant(ctx, r.Client, layout.Tenant(name), name)
		}
	}
	store, err := open(ctx, tenant.Name)
	if err != nil {
		logger.Info("usage store unavailable; the plan change is recorded on a later reconcile", "reason", err.Error())
		return
	}
	if err := store.EnsureSchema(ctx); err != nil {
		logger.Info("usage schema unavailable; the plan change is recorded on a later reconcile", "reason", err.Error())
		return
	}

	now := time.Now().UTC()
	previous := tenant.Status.ResourcePlan
	if previous == "" {
		if last, ok, err := store.LastPlanBefore(ctx, now); err != nil {
			logger.Info("usage history unreadable; the plan change is recorded on a later reconcile", "reason", err.Error())
			return
		} else if ok {
			previous = last.ToPlan
		}
	}
	event, ok := resourcePlanChange(tenant, catalogue, previous, now)
	if !ok {
		return
	}
	if event.FromPlan == event.ToPlan {
		// The history already ends on this plan: the status field is new and
		// this is the first reconcile that compares. Nothing moved.
		tenant.Status.ResourcePlan = chosen
		return
	}
	if err := store.RecordPlanEvent(ctx, event); err != nil {
		logger.Info("plan change not recorded; retried on a later reconcile", "reason", err.Error())
		return
	}
	tenant.Status.ResourcePlan = chosen
	logger.Info("resource plan change recorded", "from", event.FromPlan, "actor", event.Actor)
}
