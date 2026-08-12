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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// IntegrationBindingReconciler reconciles a IntegrationBinding object
type IntegrationBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Seeder *secrets.Seeder
}

// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch

// Reconcile reads that state of the cluster for a IntegrationBinding object and makes changes based on the state read
// and what is in the IntegrationBinding.Spec
func (r *IntegrationBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ib gentianov1alpha1.IntegrationBinding
	if err := r.Get(ctx, req.NamespacedName, &ib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.Seeder != nil {
		// Fetch Provider AppProfile
		var providerProfile gentianov1alpha1.AppProfile
		if err := r.Get(ctx, client.ObjectKey{Name: ib.Spec.Provider.App}, &providerProfile); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get provider AppProfile: %w", err)
		}

		// Fetch Tenant
		var tenant gentianov1alpha1.Tenant
		if err := r.Get(ctx, client.ObjectKey{Name: ib.Namespace}, &tenant); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get tenant: %w", err)
		}

		tenantDomain := tenant.Spec.Domain
		_ = tenantDomain // Use if needed later

		// For Nextcloud, we just set the endpoint based on the contract
		endpoint := ""
		user := fmt.Sprintf("gentian-contract-%s", ib.Spec.Contract) // A standard user
		switch ib.Spec.Contract {
		case "calendar", "contacts":
			if providerProfile.Spec.Ingress.SubDomain != "" {
				// We don't have KERNEL_DOMAIN here so we can't fully construct the URL if TenantDomain is empty
				// But we can just use the internal service URL!
				endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/remote.php/dav/",
					providerProfile.Spec.Ingress.ServiceName,
					ib.Namespace,
					providerProfile.Spec.Ingress.ServicePort)
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
					return ctrl.Result{}, fmt.Errorf("failed to write contract data to Vault: %w", err)
				}
			}
		}
	}

	ib.Status.State = gentianov1alpha1.IntegrationBindingStateReady
	if err := r.Status().Update(ctx, &ib); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IntegrationBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.IntegrationBinding{}).
		Complete(r)
}
