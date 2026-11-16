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
	"github.com/gentian-org/gentian-os/internal/placement"
)

func (r *TenantReconciler) tenantNetPolicyConfig() netpolicy.Config {
	return netpolicy.Config{
		ServicesNamespace: servicesNamespace,
		KubeAPIServerCIDR: KubeAPIServerCIDR(),
		NarrowEdge:        kernelNetworkPoliciesEnabled(),
	}
}

// KubeAPIServerCIDR is the range the API server is admitted in on 443, as the
// operator is told it (KUBE_APISERVER_CIDR), beside the addresses of the
// kubernetes Service's EndpointSlice (loadKubeAPIEndpointSlice).
func KubeAPIServerCIDR() string {
	if cidr := os.Getenv("KUBE_APISERVER_CIDR"); cidr != "" {
		return cidr
	}
	return "10.0.0.0/8"
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
// service so a NetworkPolicy can allow egress to the real apiserver addresses
// (netpolicy.BaselineEgress). Returning nil is only safe when the apiserver
// happens to sit inside KUBE_APISERVER_CIDR; on clusters reached through a
// public IP it leaves the selected pods unable to talk to the API. An error is
// logged rather than swallowed, so an RBAC gap on endpointslices is told apart
// from "no slices exist"; nil is still returned so a genuinely absent slice
// degrades rather than blocking the reconcile.
func loadKubeAPIEndpointSlice(ctx context.Context, c client.Reader) *discoveryv1.EndpointSlice {
	logger := log.FromContext(ctx)
	slices := &discoveryv1.EndpointSliceList{}
	if err := c.List(ctx, slices, client.MatchingLabels{
		kubeAPIServiceNameLabel: kubeAPIServiceName,
	}); err != nil {
		logger.Error(err, "list kube-apiserver EndpointSlices; NetworkPolicies "+
			"will only allow egress to KUBE_APISERVER_CIDR and may block the apiserver",
			"cidr", KubeAPIServerCIDR())
		return nil
	}
	if len(slices.Items) == 0 {
		logger.Info("no EndpointSlice found for the kubernetes service; "+
			"NetworkPolicies will only allow egress to KUBE_APISERVER_CIDR",
			"cidr", KubeAPIServerCIDR())
		return nil
	}
	return &slices.Items[0]
}

// The label an EndpointSlice names its Service by, and the API server's
// Service.
const (
	kubeAPIServiceNameLabel = discoveryv1.LabelServiceName
	kubeAPIServiceName      = "kubernetes"
)

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

	// Every entry's profile, the components the platform places among them:
	// a contract's two sides are selected by what their pods carry, whichever
	// side a placed component is on.
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for _, app := range tenant.Spec.Apps {
		profile, err := getProfileFor(ctx, r.Client, app.Profile, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get ComponentProfile %s for network policy: %w", app.Profile, err)
		}
		profiles[app.Profile] = profile
	}

	// A chart the platform places carries the labels its chart gives a
	// release, not the app label every other chart is rendered with. A
	// contract selects each side by what it carries.
	selectors := map[string]map[string]string{}
	var placed []string
	for name, profile := range profiles {
		if !operatorPlaced(profile) {
			continue
		}
		placed = append(placed, name)
		comp := &gentianov1alpha1.Component{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, comp); err != nil && !errors.IsNotFound(err) {
			return err
		}
		comp.Name, comp.Namespace = name, nsName
		selectors[name] = releasePods(helmReleaseOf(comp, profile))
	}

	// The per-app policies (kernel access, in-app traffic, granted egress,
	// adapters, the cache) are the installed apps'. A component the
	// platform places has its own written by its Component
	// (component_network_policy.go), and is left out here.
	installed, err := placement.Installed(ctx, r.Client, tenant.Spec.Apps)
	if err != nil {
		return err
	}

	in := netpolicy.BuildInput{
		TenantName:    tenant.Name,
		Namespace:     nsName,
		Apps:          installed,
		Placed:        placed,
		Profiles:      profiles,
		Bindings:      bindings,
		Grants:        grants,
		PodSelectors:  selectors,
		Config:        r.tenantNetPolicyConfig(),
		KubeAPIEndpts: loadKubeAPIEndpointSlice(ctx, r.Client),
	}
	desired := netpolicy.BuildDesired(in)
	desiredNames := netpolicy.ManagedPolicyNames(in)

	// Who may call the tenant's adapters is decided above; the same
	// components are told which adapters there are.
	if err := r.ensureAdapterLists(ctx, tenant, in); err != nil {
		return err
	}

	for _, np := range desired {
		if err := controllerutil.SetControllerReference(tenant, np, r.Scheme); err != nil {
			return fmt.Errorf("set owner ref on NetworkPolicy %s: %w", np.Name, err)
		}
		if err := r.applyTenantNetworkPolicy(ctx, np); err != nil {
			return err
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
		// A run's way to its external destination is the run's, removed
		// when it ends (discardRunDestinationEgress), not the tenant's.
		if t := np.Labels[meta.NetPolicyTypeLabel]; t == "" || t == meta.NetPolicyExportDestination {
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

// applyTenantNetworkPolicy writes one of the tenant's NetworkPolicies: its
// labels and spec, on every pass.
func (r *TenantReconciler) applyTenantNetworkPolicy(ctx context.Context, np *networkingv1.NetworkPolicy) error {
	if err := ensureObject(ctx, r.Client, np, func(existing *networkingv1.NetworkPolicy) bool {
		existing.Labels = np.Labels
		existing.Spec = np.Spec
		return true
	}); err != nil {
		return fmt.Errorf("ensure NetworkPolicy %s: %w", np.Name, err)
	}
	return nil
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
