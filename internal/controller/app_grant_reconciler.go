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
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/catalogue"
)

const (
	conditionAppGrantReady = "AppGrantReady"
	appGrantRequeue        = 2 * time.Minute
)

// AppGrantReconciler syncs AppGrant objects to OpenFGA tuples.
type AppGrantReconciler struct {
	client.Client
	OpenFGAURL   string
	OpenFGAToken string
	Enabled      bool
	storeID      string
}

func (r *AppGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	grant := &gentianov1alpha1.AppGrant{}
	if err := r.Get(ctx, req.NamespacedName, grant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tenantName := grant.Labels[tenantLabel]
	if tenantName == "" {
		tenantName = strings.TrimPrefix(req.Namespace, "tenant-")
	}

	if !r.Enabled {
		setAppGrantCondition(grant, conditionAppGrantReady, metav1.ConditionTrue, "AuthzBridgeDisabled",
			"OpenFGA bridge disabled; grant recorded in Kubernetes only")
		grant.Status.Phase = gentianov1alpha1.AppGrantPhaseReady
		grant.Status.ObservedGeneration = grant.Generation
		return ctrl.Result{}, r.Status().Update(ctx, grant)
	}

	if err := r.ensureStore(ctx); err != nil {
		setAppGrantCondition(grant, conditionAppGrantReady, metav1.ConditionFalse, "OpenFGABootstrapFailed", err.Error())
		grant.Status.Phase = gentianov1alpha1.AppGrantPhaseDegraded
		_ = r.Status().Update(ctx, grant)
		return ctrl.Result{RequeueAfter: appGrantRequeue}, err
	}

	tuples := authz.GrantTuples(tenantName, grant)
	fga := authz.NewOpenFGAClient(r.OpenFGAURL, r.OpenFGAToken)
	if err := fga.WriteTuples(ctx, r.storeID, tuples, nil); err != nil {
		setAppGrantCondition(grant, conditionAppGrantReady, metav1.ConditionFalse, "TupleSyncFailed", err.Error())
		grant.Status.Phase = gentianov1alpha1.AppGrantPhaseDegraded
		_ = r.Status().Update(ctx, grant)
		return ctrl.Result{RequeueAfter: appGrantRequeue}, err
	}

	grant.Status.TupleCount = len(tuples)
	grant.Status.Phase = gentianov1alpha1.AppGrantPhaseReady
	grant.Status.ObservedGeneration = grant.Generation
	setAppGrantCondition(grant, conditionAppGrantReady, metav1.ConditionTrue, "Synced",
		fmt.Sprintf("Synced %d OpenFGA tuples", len(tuples)))
	if err := r.Status().Update(ctx, grant); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("app grant synced", "tenant", tenantName, "app", grant.Spec.App, "tuples", len(tuples))
	return ctrl.Result{RequeueAfter: appGrantRequeue}, nil
}

func (r *AppGrantReconciler) ensureStore(ctx context.Context) error {
	if r.storeID != "" {
		return nil
	}
	bridge := &authz.Bridge{
		OpenFGA: authz.NewOpenFGAClient(r.OpenFGAURL, r.OpenFGAToken),
	}
	if err := bridge.EnsureBootstrap(ctx); err != nil {
		return err
	}
	r.storeID = bridge.StoreID
	return nil
}

func (r *AppGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.AppGrant{}).
		Complete(r)
}

func setAppGrantCondition(grant *gentianov1alpha1.AppGrant, typ string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range grant.Status.Conditions {
		if grant.Status.Conditions[i].Type == typ {
			grant.Status.Conditions[i].Status = status
			grant.Status.Conditions[i].Reason = reason
			grant.Status.Conditions[i].Message = message
			grant.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	grant.Status.Conditions = append(grant.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// ensureAppGrants creates or updates default AppGrant CRs for installed apps.
func (r *TenantReconciler) ensureAppGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	nsName := tenantNamespaceName(tenant)
	desired, err := r.collectDesiredAppGrants(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(desired) == 0 {
		if err := r.gcStaleAppGrants(ctx, nsName, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	desiredMap := make(map[string]*gentianov1alpha1.AppGrant, len(desired))
	for _, g := range desired {
		desiredMap[g.Name] = g
		existing := &gentianov1alpha1.AppGrant{}
		err := r.Get(ctx, types.NamespacedName{Name: g.Name, Namespace: g.Namespace}, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, g); err != nil {
				return ctrl.Result{}, fmt.Errorf("create AppGrant %s: %w", g.Name, err)
			}
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get AppGrant %s: %w", g.Name, err)
		}
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = g.Spec
		existing.Labels = g.Labels
		if err := r.Patch(ctx, existing, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch AppGrant %s: %w", g.Name, err)
		}
	}

	if err := r.gcStaleAppGrants(ctx, nsName, desiredMap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) collectDesiredAppGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]*gentianov1alpha1.AppGrant, error) {
	nsName := tenantNamespaceName(tenant)
	bindings, err := r.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	consumeByApp := map[string][]gentianov1alpha1.ConsumeGrantSpec{}
	for _, ib := range bindings {
		granted := ib.Spec.Capabilities
		if len(granted) == 0 {
			continue
		}
		consumeByApp[ib.Spec.Consumer.App] = append(consumeByApp[ib.Spec.Consumer.App], gentianov1alpha1.ConsumeGrantSpec{
			Contract: ib.Spec.Contract,
			Granted:  append([]string(nil), granted...),
		})
	}

	var out []*gentianov1alpha1.AppGrant
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return nil, err
		}
		consume := consumeByApp[profileName]
		if len(consume) == 0 {
			continue
		}
		out = append(out, buildAppGrant(nsName, tenant.Name, profileName, consume, nil))
	}
	return out, nil
}

func buildAppGrant(
	nsName, tenantName, app string,
	consume []gentianov1alpha1.ConsumeGrantSpec,
	allow []gentianov1alpha1.AllowConsumerSpec,
) *gentianov1alpha1.AppGrant {
	return &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appGrantName(app),
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenantName,
				appLabel:       app,
				managedByLabel: managedByValue,
			},
		},
		Spec: gentianov1alpha1.AppGrantSpec{
			App:            app,
			Consume:        consume,
			AllowConsumers: allow,
		},
	}
}

func appGrantName(app string) string {
	return app
}

func (r *TenantReconciler) gcStaleAppGrants(
	ctx context.Context,
	nsName string,
	desired map[string]*gentianov1alpha1.AppGrant,
) error {
	existing := &gentianov1alpha1.AppGrantList{}
	if err := r.List(ctx, existing,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue},
	); err != nil {
		return fmt.Errorf("list AppGrants in %s: %w", nsName, err)
	}
	for i := range existing.Items {
		ag := &existing.Items[i]
		if desired != nil {
			if _, keep := desired[ag.Name]; keep {
				continue
			}
		}
		if err := r.Delete(ctx, ag); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale AppGrant %s: %w", ag.Name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) deleteAppGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.gcStaleAppGrants(ctx, tenantNamespaceName(tenant), nil)
}
