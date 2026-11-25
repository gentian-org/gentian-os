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
	stderrors "errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

// KubeAPIServerCIDR is the range the API server is admitted in on 443 when
// none of its addresses can be read, as the operator is told it
// (KUBE_APISERVER_CIDR). With an address read it is not admitted at all
// (netpolicy.APIServerEgress).
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

// errDNSNotNarrowed and errAPIServerNotNarrowed are what the log says when
// a policy cannot be narrowed to the cluster's own services.
var (
	errDNSNotNarrowed = stderrors.New("no DNS Service found in " + netpolicy.DNSNamespace +
		": port 53 stays open to every address")
	errAPIServerNotNarrowed = stderrors.New("no address of the API server read: " +
		"KUBE_APISERVER_CIDR is admitted on 443")
)

// loadClusterServices reads the cluster's DNS and API server for the
// policies that admit them (netpolicy.BaselineEgress, netpolicy.DNSEgress).
//
// The DNS is every Service in kube-system that serves port 53 and selects
// its pods. When none is found the rules leave port 53 open to every
// address, and the refusal to narrow is logged: closing DNS would stop every
// tenant. The API server is the kubernetes Service in default and its
// EndpointSlices; when no address of it is read the rules admit
// KUBE_APISERVER_CIDR on 443 instead, and that is logged too.
//
// A read that fails is logged and taken as nothing found, so an RBAC gap is
// told apart from a cluster that has no such Service, and the reconcile goes
// on.
func loadClusterServices(ctx context.Context, c client.Reader) netpolicy.ClusterServices {
	logger := log.FromContext(ctx)
	out := netpolicy.ClusterServices{DNS: loadClusterDNS(ctx, c)}

	api := &corev1.Service{}
	key := types.NamespacedName{Name: netpolicy.APIServerServiceName, Namespace: metav1.NamespaceDefault}
	if err := c.Get(ctx, key, api); err != nil {
		logger.Error(err, "get the kubernetes Service for the API server's cluster address")
	} else {
		out.APIService = api
	}
	slices := &discoveryv1.EndpointSliceList{}
	if err := c.List(ctx, slices, client.InNamespace(metav1.NamespaceDefault), client.MatchingLabels{
		kubeAPIServiceNameLabel: kubeAPIServiceName,
	}); err != nil {
		logger.Error(err, "list the kubernetes Service's EndpointSlices for the API server's addresses")
	} else {
		out.APIEndpoints = slices.Items
	}
	if !out.HasAPIEndpoints() {
		logger.Error(errAPIServerNotNarrowed, "network policies fall back to a range for the API server",
			"cidr", KubeAPIServerCIDR())
	}
	return out
}

// loadClusterDNS is the DNS half of loadClusterServices, for a policy that
// admits the cluster's DNS and not the API server.
func loadClusterDNS(ctx context.Context, c client.Reader) []corev1.Service {
	logger := log.FromContext(ctx)
	var dns []corev1.Service
	services := &corev1.ServiceList{}
	if err := c.List(ctx, services, client.InNamespace(netpolicy.DNSNamespace)); err != nil {
		logger.Error(err, "list the Services in "+netpolicy.DNSNamespace+" for the cluster's DNS")
	} else {
		for i := range services.Items {
			if netpolicy.ServesDNS(&services.Items[i]) {
				dns = append(dns, services.Items[i])
			}
		}
	}
	if len(dns) == 0 {
		logger.Error(errDNSNotNarrowed, "network policies refuse to narrow DNS")
	}
	return dns
}

// The label an EndpointSlice names its Service by, and the API server's
// Service.
const (
	kubeAPIServiceNameLabel = discoveryv1.LabelServiceName
	kubeAPIServiceName      = netpolicy.APIServerServiceName
)

// clusterServiceChanged passes what loadClusterServices reads, when what it
// reads of it changes: a DNS Service in kube-system (whether it serves DNS,
// its selector, ports and cluster addresses), the kubernetes Service, and
// the kubernetes Service's EndpointSlices (addresses and ports). A cluster
// that gains its DNS Service after the operator starts is told by the
// Service's creation.
func clusterServiceChanged() predicate.Predicate {
	return clusterFactsChanged(func(o client.Object) bool {
		switch obj := o.(type) {
		case *corev1.Service:
			return netpolicy.ServesDNS(obj) ||
				(obj.Namespace == metav1.NamespaceDefault && obj.Name == netpolicy.APIServerServiceName)
		case *discoveryv1.EndpointSlice:
			return obj.Namespace == metav1.NamespaceDefault && obj.Labels[kubeAPIServiceNameLabel] == kubeAPIServiceName
		}
		return false
	})
}

// clusterDNSChanged is clusterServiceChanged for the DNS Services alone
// (loadClusterDNS).
func clusterDNSChanged() predicate.Predicate {
	return clusterFactsChanged(func(o client.Object) bool {
		svc, ok := o.(*corev1.Service)
		return ok && netpolicy.ServesDNS(svc)
	})
}

func clusterFactsChanged(relevant func(client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			// A Service that stops serving DNS is a change as much as one
			// that starts.
			if !relevant(e.ObjectOld) && !relevant(e.ObjectNew) {
				return false
			}
			switch n := e.ObjectNew.(type) {
			case *corev1.Service:
				o, ok := e.ObjectOld.(*corev1.Service)
				return !ok || netpolicy.ServesDNS(o) != netpolicy.ServesDNS(n) ||
					!equality.Semantic.DeepEqual(o.Spec.Selector, n.Spec.Selector) ||
					!equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports) ||
					!equality.Semantic.DeepEqual(o.Spec.ClusterIPs, n.Spec.ClusterIPs) ||
					o.Spec.ClusterIP != n.Spec.ClusterIP
			case *discoveryv1.EndpointSlice:
				o, ok := e.ObjectOld.(*discoveryv1.EndpointSlice)
				return !ok || !equality.Semantic.DeepEqual(o.Endpoints, n.Endpoints) ||
					!equality.Semantic.DeepEqual(o.Ports, n.Ports)
			}
			return true
		},
	}
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
	// agent bridges, the cache) are the installed apps'. A component the
	// platform places has its own written by its Component
	// (component_network_policy.go), and is left out here.
	installed, err := placement.Installed(ctx, r.Client, tenant.Spec.Apps)
	if err != nil {
		return err
	}

	// An app whose optional model gateway requirement is not met has no path
	// to the gateway, as its release has no address and no key.
	withheld := map[string]bool{}
	for _, app := range installed {
		profile := profiles[app.Profile]
		if profile == nil {
			continue
		}
		without, err := modelAccessWithheld(ctx, r.Client, nsName, app.Profile, profile, tenant)
		if err != nil {
			return fmt.Errorf("read whether %s holds a key at the model gateway: %w", app.Profile, err)
		}
		if without {
			withheld[app.Profile] = true
		}
	}

	in := netpolicy.BuildInput{
		TenantName:          tenant.Name,
		Namespace:           nsName,
		Apps:                installed,
		Placed:              placed,
		Profiles:            profiles,
		Bindings:            bindings,
		BridgeGrants:        tenant.Spec.Privileges,
		Grants:              grants,
		PodSelectors:        selectors,
		ModelAccessWithheld: withheld,
		Config:              r.tenantNetPolicyConfig(),
		Cluster:             loadClusterServices(ctx, r.Client),
	}
	desired := netpolicy.BuildDesired(in)
	desiredNames := netpolicy.ManagedPolicyNames(in)

	// Who may call the tenant's agent bridges is decided above; the same
	// components are told which bridges there are.
	if err := r.ensureAgentBridgeLists(ctx, tenant, in); err != nil {
		return err
	}
	// What the tenant's home sync keeps equal follows the same installs.
	if err := r.ensureHomeSyncPlan(ctx, tenant, in); err != nil {
		return err
	}
	// And what the tenant's file dialog serves, and to which apps.
	if err := r.ensurePorterPlan(ctx, tenant, in); err != nil {
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
