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
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// IntegrationBindingReconciler reconciles a IntegrationBinding object
type IntegrationBindingReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	Scheme *runtime.Scheme
	Seeder *secrets.Seeder
}

// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=appgrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=componentprofiles,verbs=get;list;watch

// Reconcile reads that state of the cluster for a IntegrationBinding object and makes changes based on the state read
// and what is in the IntegrationBinding.Spec
func (r *IntegrationBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ib gentianov1alpha1.IntegrationBinding
	if err := r.Get(ctx, req.NamespacedName, &ib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// What was granted is said whether or not the wiring below succeeds: a
	// binding nobody granted opens nothing, and a reader has to be able to
	// see that this, and no fault, is why the two apps cannot reach each
	// other.
	granted, err := r.grantedCondition(ctx, &ib)
	if err != nil {
		return ctrl.Result{}, err
	}
	apimeta.SetStatusCondition(&ib.Status.Conditions, granted)

	seedErr := r.seedContract(ctx, &ib)
	if seedErr == nil {
		ib.Status.State = gentianov1alpha1.IntegrationBindingStateReady
	}
	if err := r.Status().Update(ctx, &ib); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, seedErr
}

// conditionBindingGranted says how much of what a binding asks for the
// tenant's administrator granted.
const conditionBindingGranted = "Granted"

// grantedCondition compares what the binding asks for with the consumer's
// grant. True only when every capability asked for was granted.
func (r *IntegrationBindingReconciler) grantedCondition(ctx context.Context, ib *gentianov1alpha1.IntegrationBinding) (metav1.Condition, error) {
	var grants gentianov1alpha1.AppGrantList
	if err := r.List(ctx, &grants, client.InNamespace(ib.Namespace)); err != nil {
		return metav1.Condition{}, fmt.Errorf("list AppGrants in %s: %w", ib.Namespace, err)
	}
	var grant *gentianov1alpha1.AppGrant
	for i := range grants.Items {
		if grants.Items[i].Spec.App == ib.Spec.Consumer.App {
			grant = &grants.Items[i]
		}
	}
	effective := netpolicy.EffectiveContractCapabilities(ib, grant)
	have := make(map[string]struct{}, len(effective))
	for _, c := range effective {
		have[c] = struct{}{}
	}
	var missing []string
	for _, c := range ib.Spec.Capabilities {
		if _, ok := have[c]; !ok {
			missing = append(missing, c)
		}
	}
	cond := metav1.Condition{Type: conditionBindingGranted, ObservedGeneration: ib.Generation}
	switch {
	case len(effective) == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotGranted"
		cond.Message = fmt.Sprintf("%s asks for contract %s of %s and was granted none of it: nothing is opened between the two until the tenant's administrator grants it",
			ib.Spec.Consumer.App, ib.Spec.Contract, ib.Spec.Provider.App)
	case len(missing) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PartlyGranted"
		cond.Message = fmt.Sprintf("granted: %s; asked for and not granted: %s",
			strings.Join(effective, ", "), strings.Join(missing, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Granted"
		cond.Message = "granted: " + strings.Join(effective, ", ")
	}
	return cond, nil
}

// seedContract writes what the consumer needs to address the provider.
func (r *IntegrationBindingReconciler) seedContract(ctx context.Context, ib *gentianov1alpha1.IntegrationBinding) error {
	if r.Seeder == nil {
		return nil
	}
	// The provider's profile, for the Service its contract is served on.
	//
	// A ComponentProfile: AD-4 leaves one catalogue kind, and this read
	// asked for the other one long after the catalogue stopped shipping
	// it -- so every binding failed here with a NotFound and no contract
	// credential was ever seeded. Found by deleting the AppProfile type
	// rather than by anything noticing at runtime, which is the argument
	// for deleting a type instead of leaving it defined and unused.
	var providerProfile gentianov1alpha1.ComponentProfile
	if err := r.Get(ctx, client.ObjectKey{Name: ib.Spec.Provider.App}, &providerProfile); err != nil {
		return fmt.Errorf("failed to get provider ComponentProfile: %w", err)
	}

	// Fetch Tenant
	var tenant gentianov1alpha1.Tenant
	if err := r.Get(ctx, client.ObjectKey{Name: ib.Namespace}, &tenant); err != nil {
		return fmt.Errorf("failed to get tenant: %w", err)
	}

	// For Nextcloud, we just set the endpoint based on the contract
	endpoint := ""
	user := fmt.Sprintf("gentian-contract-%s", ib.Spec.Contract) // A standard user
	switch ib.Spec.Contract {
	case "calendar", "contacts":
		// The provider's own web entry, which is what its DAV endpoint is
		// served on. Its backend names the Service and the port; where
		// the entry routes to ANOTHER component, that component's Service
		// is the one to address, because that is where the contract is
		// actually answered.
		//
		// Addressed in-cluster rather than through the gateway: the
		// consumer is in the same namespace, the endpoint is for a
		// machine, and a round trip out to the edge and back would need a
		// session this has no business holding.
		for _, e := range providerProfile.GatewayExposures() {
			if e.Name != primaryExposureName || e.Backend.Service == "" {
				continue
			}
			endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/remote.php/dav/",
				e.Backend.Service, ib.Namespace, e.Backend.Port)
			break
		}
	}

	if endpoint != "" {
		path := secrets.ContractPath(ib.Namespace, ib.Spec.Contract)
		data, _ := r.Seeder.Read(ctx, path)
		if data == nil {
			data = make(map[string]string)
		}
		needsUpdate := false
		if data["endpoint"] != endpoint {
			data["endpoint"] = endpoint
			needsUpdate = true
		}
		if data["user"] != user {
			data["user"] = user
			needsUpdate = true
		}
		if needsUpdate {
			if err := r.Seeder.Write(ctx, path, data); err != nil {
				return fmt.Errorf("failed to write contract data to Vault: %w", err)
			}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IntegrationBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A grant that is made, changed or withdrawn changes what every binding
	// of its namespace has to say about itself.
	bindingsOfGrant := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var bindings gentianov1alpha1.IntegrationBindingList
		if err := r.List(ctx, &bindings, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(bindings.Items))
		for i := range bindings.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings.Items[i])})
		}
		return out
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.IntegrationBinding{}).
		Watches(&gentianov1alpha1.AppGrant{}, handler.EnqueueRequestsFromMapFunc(bindingsOfGrant)).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.IntegrationBinding{}, r))
}
