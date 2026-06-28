/*
Copyright 2026 The Gentian Authors.

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
	"os"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func (r *TenantReconciler) tenantNetPolicyConfig() netpolicy.Config {
	stage := envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
	infraNS := os.Getenv("INFRA_NAMESPACE")
	if infraNS == "" {
		infraNS = "gentian-infra-" + stage
	}
	cidr := os.Getenv("KUBE_APISERVER_CIDR")
	if cidr == "" {
		cidr = "10.96.0.0/12"
	}
	return netpolicy.Config{
		InfraNamespace:    infraNS,
		ServicesNamespace: servicesNamespace,
		OpenbaoNamespace:  "openbao",
		RoutingMode:       r.RoutingMode,
		KubeAPIServerCIDR: cidr,
	}
}

func (r *TenantReconciler) loadKubeAPIEndpointSlice(ctx context.Context) *discoveryv1.EndpointSlice {
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices, client.MatchingLabels{
		"kubernetes.io/service-name": "kubernetes",
	}); err != nil || len(slices.Items) == 0 {
		return nil
	}
	return &slices.Items[0]
}

func (r *TenantReconciler) ensureNetworkPolicies(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	bindings, err := r.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		return err
	}

	profiles := map[string]*gentianov1alpha1.AppProfile{}
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get AppProfile %s for network policy: %w", app.Profile, err)
		}
		profiles[app.Profile] = profile
	}

	in := netpolicy.BuildInput{
		TenantName:    tenant.Name,
		Namespace:     nsName,
		Apps:          tenant.Spec.Apps,
		Profiles:      profiles,
		Bindings:      bindings,
		Config:        r.tenantNetPolicyConfig(),
		KubeAPIEndpts: r.loadKubeAPIEndpointSlice(ctx),
	}
	desired := netpolicy.BuildDesired(in)
	desiredNames := netpolicy.ManagedPolicyNames(in)

	for _, np := range desired {
		np := np
		if err := controllerutil.SetControllerReference(tenant, np, r.Scheme); err != nil {
			return fmt.Errorf("set owner ref on NetworkPolicy %s: %w", np.Name, err)
		}
		existing := &networkingv1.NetworkPolicy{}
		err := r.Get(ctx, types.NamespacedName{Name: np.Name, Namespace: np.Namespace}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, np); err != nil {
				return fmt.Errorf("create NetworkPolicy %s: %w", np.Name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("get NetworkPolicy %s: %w", np.Name, err)
		}
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Labels = np.Labels
		existing.Spec = np.Spec
		if err := r.Patch(ctx, existing, patch); err != nil {
			return fmt.Errorf("patch NetworkPolicy %s: %w", np.Name, err)
		}
	}

	existingList := &networkingv1.NetworkPolicyList{}
	if err := r.List(ctx, existingList,
		client.InNamespace(nsName),
		client.MatchingLabels{meta.ManagedByLabel: meta.ManagedByValue},
	); err != nil {
		return fmt.Errorf("list managed NetworkPolicies in %s: %w", nsName, err)
	}
	for i := range existingList.Items {
		np := &existingList.Items[i]
		if np.Labels[meta.NetPolicyTypeLabel] == "" {
			continue
		}
		if _, keep := desiredNames[np.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, np); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale NetworkPolicy %s: %w", np.Name, err)
		}
	}
	return nil
}
