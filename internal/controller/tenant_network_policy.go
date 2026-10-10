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
	"os"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func (r *TenantReconciler) tenantNetPolicyConfig() netpolicy.Config {
	cidr := os.Getenv("KUBE_APISERVER_CIDR")
	if cidr == "" {
		cidr = "10.0.0.0/8"
	}
	return netpolicy.Config{
		ServicesNamespace: servicesNamespace,
		RoutingMode:       r.RoutingMode,
		KubeAPIServerCIDR: cidr,
		NarrowEdge:        kernelNetworkPoliciesEnabled(),
	}
}

// kernelNetworkPoliciesEnabled reports the cluster's switch for the kernel's
// network rules, as the operator is told it (KERNEL_NETWORK_POLICIES, from
// the installer through the chart). The installer applies and removes the
// kernel namespaces' own policies by it; the operator's two rules that lean
// on the same fact -- which pods of the edge namespace are the Gateway's --
// follow it: a tenant's baseline admits those pods alone, and a publishing
// proxy admits them alone. Anything but "true" is off.
func kernelNetworkPoliciesEnabled() bool {
	return os.Getenv("KERNEL_NETWORK_POLICIES") == "true"
}

// loadKubeAPIEndpointSlice returns the EndpointSlice backing the "kubernetes"
// service so the baseline NetworkPolicy can allow egress to the real apiserver
// addresses. Returning nil is only safe when the apiserver happens to sit inside
// KUBE_APISERVER_CIDR; on clusters reached through a public IP it silently
// leaves tenants unable to talk to the API. It used to swallow the error, so an
// RBAC gap on endpointslices was indistinguishable from "no slices exist" —
// both paths just returned nil. Log loudly instead: nil is still returned so a
// genuinely absent slice degrades rather than blocking the reconcile.
func (r *TenantReconciler) loadKubeAPIEndpointSlice(ctx context.Context) *discoveryv1.EndpointSlice {
	logger := log.FromContext(ctx)
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices, client.MatchingLabels{
		"kubernetes.io/service-name": "kubernetes",
	}); err != nil {
		logger.Error(err, "list kube-apiserver EndpointSlices; tenant NetworkPolicies "+
			"will only allow egress to KUBE_APISERVER_CIDR and may block the apiserver",
			"cidr", r.tenantNetPolicyConfig().KubeAPIServerCIDR)
		return nil
	}
	if len(slices.Items) == 0 {
		logger.Info("no EndpointSlice found for the kubernetes service; tenant "+
			"NetworkPolicies will only allow egress to KUBE_APISERVER_CIDR",
			"cidr", r.tenantNetPolicyConfig().KubeAPIServerCIDR)
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

	grants, err := r.loadAppGrantsByConsumer(ctx, nsName)
	if err != nil {
		return err
	}

	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get ComponentProfile %s for network policy: %w", app.Profile, err)
		}
		profiles[app.Profile] = profile
	}

	// An app whose chart the operator installs itself carries the labels
	// its chart gives a release, not the app label the Composition puts on
	// the pods it renders. A contract selects each side by what it carries.
	selectors := map[string]map[string]string{}
	for name, profile := range profiles {
		if !composedDelivery(profile) {
			selectors[name] = map[string]string{componentInstanceLabel: nsName + "-" + name}
		}
	}

	in := netpolicy.BuildInput{
		TenantName:    tenant.Name,
		Namespace:     nsName,
		Apps:          tenant.Spec.Apps,
		Profiles:      profiles,
		Bindings:      bindings,
		Grants:        grants,
		PodSelectors:  selectors,
		Config:        r.tenantNetPolicyConfig(),
		KubeAPIEndpts: r.loadKubeAPIEndpointSlice(ctx),
	}
	desired := netpolicy.BuildDesired(in)
	desiredNames := netpolicy.ManagedPolicyNames(in)

	for _, np := range desired {
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
	// With the way between two apps goes the means to tell who is calling,
	// and both come and go with the grant.
	return r.ensureContractKeys(ctx, tenant, nsName, bindings, grants)
}

func (r *TenantReconciler) loadAppGrantsByConsumer(ctx context.Context, nsName string) (map[string]*gentianov1alpha1.AppGrant, error) {
	list := &gentianov1alpha1.AppGrantList{}
	if err := r.List(ctx, list, client.InNamespace(nsName)); err != nil {
		return nil, fmt.Errorf("list AppGrants in %s: %w", nsName, err)
	}
	out := make(map[string]*gentianov1alpha1.AppGrant, len(list.Items))
	for i := range list.Items {
		ag := &list.Items[i]
		if ag.Spec.App == "" {
			continue
		}
		out[ag.Spec.App] = ag
	}
	return out, nil
}
