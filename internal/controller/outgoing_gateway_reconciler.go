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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/dispatcher"
	"github.com/gentian-org/gentian-os/internal/egress"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete

// envoyGatewayPods are Envoy Gateway's own pods and envoyGatewayConfigPort
// the port its proxies fetch their configuration at (the kernel's inventory,
// workload envoy-gateway).
var envoyGatewayPods = map[string]string{"control-plane": "envoy-gateway"}

const envoyGatewayConfigPort int32 = 18000

// outgoingReconcileKey is the one request this reconciler works on.
const outgoingReconcileKey = "outgoing-gateway"

// OutgoingGatewayReconciler keeps, in the edge namespace, what carries the
// components' connections to hosts outside the cluster.
//
// Every Component says in its status which hosts it was granted. From all of
// them this builds the Gateways of the outgoing class with a listener per
// host, the route and the Services of each host, the policy that admits to a
// listener the pods granted its host, and the fleet's own policy for what it
// may call (internal/egress). What no Component lists any more is removed,
// so a grant that went takes the host's listener with it.
//
// It writes nothing while the outgoing GatewayClass is missing: on a cluster
// the installer has not put it on, a Gateway of that class would wait for a
// controller forever. A component with a granted host says so in its own
// condition (OutgoingGatewayPending).
type OutgoingGatewayReconciler struct {
	client.Client
}

func (r *OutgoingGatewayReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	edge := layout.Namespace(layout.Edge)

	grants, err := r.grants(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(grants) > 0 {
		class := &gatewayv1.GatewayClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: netpolicy.OutgoingGatewayClassName}, class); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("hosts outside are granted and the cluster has no GatewayClass for outgoing connections; nothing is carried until the installer's step A-05 applies it",
					"gatewayClass", netpolicy.OutgoingGatewayClassName, "hosts", len(grants))
				return reconcile.Result{RequeueAfter: platformRetryAfter}, nil
			}
			return reconcile.Result{}, err
		}
	}

	existing, err := r.listenerPorts(ctx, edge)
	if err != nil {
		return reconcile.Result{}, err
	}
	hosts := make([]egress.Host, 0, len(grants))
	for i := range grants {
		hosts = append(hosts, grants[i].Host)
	}
	ports, err := egress.Allocate(existing, hosts)
	if err != nil {
		return reconcile.Result{}, err
	}
	desired := egress.Desired(edge, grants, ports)

	// The fleet's own rule first, and with every port the hosts need: a
	// listener that exists before the fleet may leave on its host's port
	// refuses, which is the safe order.
	if len(hosts) > 0 {
		policy := egress.FleetPolicy(edge, envoyGatewayPods, envoyGatewayConfigPort, hosts)
		if err := ensureOutgoingObject(ctx, r.Client, policy, func(have *networkingv1.NetworkPolicy) bool {
			return adopt(&have.Spec, policy.Spec)
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("the fleet's policy: %w", err)
		}
	}
	keep := map[string]bool{"NetworkPolicy/" + egress.FleetPolicyName: len(hosts) > 0}
	for _, np := range desired.NetworkPolicies {
		keep["NetworkPolicy/"+np.Name] = true
		if err := ensureOutgoingObject(ctx, r.Client, np, func(have *networkingv1.NetworkPolicy) bool {
			return adopt(&have.Spec, np.Spec)
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("policy %s: %w", np.Name, err)
		}
	}
	for _, svc := range desired.Services {
		keep["Service/"+svc.Name] = true
		if err := ensureOutgoingObject(ctx, r.Client, svc, func(have *corev1.Service) bool {
			// The address is the Service's own and stays: the callers' host
			// tables name it.
			changed := adopt(&have.Spec.Ports, svc.Spec.Ports)
			return adopt(&have.Spec.Selector, svc.Spec.Selector) || changed
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("service %s: %w", svc.Name, err)
		}
	}
	for _, slice := range desired.EndpointSlices {
		keep["EndpointSlice/"+slice.Name] = true
		if err := ensureOutgoingObject(ctx, r.Client, slice, func(have *discoveryv1.EndpointSlice) bool {
			changed := adopt(&have.Endpoints, slice.Endpoints)
			return adopt(&have.Ports, slice.Ports) || changed
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("endpoint %s: %w", slice.Name, err)
		}
	}
	for _, gw := range desired.Gateways {
		keep["Gateway/"+gw.Name] = true
		if err := ensureOutgoingObject(ctx, r.Client, gw, func(have *gatewayv1.Gateway) bool {
			return adopt(&have.Spec, gw.Spec)
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("gateway %s: %w", gw.Name, err)
		}
	}
	for _, route := range desired.Routes {
		keep["TLSRoute/"+route.Name] = true
		if err := ensureOutgoingObject(ctx, r.Client, route, func(have *gatewayv1.TLSRoute) bool {
			return adopt(&have.Spec, route.Spec)
		}); err != nil {
			return reconcile.Result{}, fmt.Errorf("route %s: %w", route.Name, err)
		}
	}
	return reconcile.Result{}, r.prune(ctx, edge, keep)
}

// adopt sets have to want and says whether it differed.
func adopt[T any](have *T, want T) bool {
	if equality.Semantic.DeepEqual(*have, want) {
		return false
	}
	*have = want
	return true
}

// ensureOutgoingObject is ensureObject for an object of this reconciler: one
// already there is changed only when it carries the mark, so an object of
// the same name that somebody else made is left alone and reported.
func ensureOutgoingObject[T client.Object](ctx context.Context, c client.Client, desired T, mutate func(existing T) bool) error {
	return ensureObject(ctx, c, desired, func(existing T) bool {
		if existing.GetLabels()[egress.ManagedLabel] != "true" {
			log.FromContext(ctx).Info("an object of this name exists and is not the outgoing gateway's; left as it is",
				"name", desired.GetName(), "namespace", desired.GetNamespace())
			return false
		}
		return mutate(existing)
	})
}

// grants are the hosts every Component lists in its status, each with the
// pods that may call it: the component's own, by the labels the operator
// sets on every pod template of its release (apprender, the label rule), or,
// for an installed action, the pods of its runs in the tenant's namespace
// for runs, by the label the dispatcher starts them with.
func (r *OutgoingGatewayReconciler) grants(ctx context.Context) ([]egress.Grant, error) {
	comps := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, comps); err != nil {
		return nil, err
	}
	var tenants *gentianov1alpha1.TenantList
	byHost := map[egress.Host]*egress.Grant{}
	var order []egress.Host
	for i := range comps.Items {
		comp := &comps.Items[i]
		if !comp.DeletionTimestamp.IsZero() || len(comp.Status.OutgoingHosts) == 0 {
			continue
		}
		caller := egress.Caller{Namespace: comp.Namespace, PodLabels: map[string]string{appLabel: comp.Name}}
		if comp.Spec.Class == gentianov1alpha1.ComponentClassAction {
			if tenants == nil {
				tenants = &gentianov1alpha1.TenantList{}
				if err := r.List(ctx, tenants); err != nil {
					return nil, err
				}
			}
			holder := gentianov1alpha1.TenantInNamespace(tenants.Items, comp.Namespace)
			if holder == nil {
				continue
			}
			caller = egress.Caller{
				Namespace: layout.TenantRuns(holder.Name),
				PodLabels: map[string]string{dispatcher.LabelAction: comp.Name},
			}
		}
		for _, s := range comp.Status.OutgoingHosts {
			h, err := egress.ParseHost(s)
			if err != nil {
				log.FromContext(ctx).Info("a host in a component's status is not a host outside; not carried",
					"component", comp.Name, "namespace", comp.Namespace, "host", s)
				continue
			}
			g := byHost[h]
			if g == nil {
				g = &egress.Grant{Host: h}
				byHost[h] = g
				order = append(order, h)
			}
			g.Callers = append(g.Callers, caller)
		}
	}
	out := make([]egress.Grant, 0, len(order))
	for _, h := range order {
		out = append(out, *byHost[h])
	}
	return out, nil
}

// listenerPorts reads the listener port every carried host has, off the
// Services the callers connect to: each port of such a Service forwards a
// host's own port to its listener.
func (r *OutgoingGatewayReconciler) listenerPorts(ctx context.Context, edge string) (map[egress.Host]int32, error) {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(edge), client.MatchingLabels{egress.ManagedLabel: "true"}); err != nil {
		return nil, err
	}
	out := map[egress.Host]int32{}
	for i := range services.Items {
		svc := &services.Items[i]
		name := svc.Annotations[egress.HostAnnotation]
		if name == "" || len(svc.Spec.Selector) == 0 {
			continue
		}
		for _, p := range svc.Spec.Ports {
			if p.TargetPort.IntVal > 0 {
				out[egress.Host{Name: name, Port: p.Port}] = p.TargetPort.IntVal
			}
		}
	}
	return out, nil
}

// prune removes what this reconciler made and no longer wants. Routes and
// Gateways go first, so that a listener is gone before the Service and the
// policy of its host are.
func (r *OutgoingGatewayReconciler) prune(ctx context.Context, edge string, keep map[string]bool) error {
	mine := []client.ListOption{client.InNamespace(edge), client.MatchingLabels{egress.ManagedLabel: "true"}}
	routes := &gatewayv1.TLSRouteList{}
	if err := r.List(ctx, routes, mine...); err != nil {
		return err
	}
	for i := range routes.Items {
		if !keep["TLSRoute/"+routes.Items[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &routes.Items[i])); err != nil {
				return err
			}
		}
	}
	gateways := &gatewayv1.GatewayList{}
	if err := r.List(ctx, gateways, mine...); err != nil {
		return err
	}
	for i := range gateways.Items {
		if !keep["Gateway/"+gateways.Items[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &gateways.Items[i])); err != nil {
				return err
			}
		}
	}
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices, mine...); err != nil {
		return err
	}
	for i := range slices.Items {
		if !keep["EndpointSlice/"+slices.Items[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &slices.Items[i])); err != nil {
				return err
			}
		}
	}
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, mine...); err != nil {
		return err
	}
	for i := range services.Items {
		if !keep["Service/"+services.Items[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &services.Items[i])); err != nil {
				return err
			}
		}
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := r.List(ctx, policies, mine...); err != nil {
		return err
	}
	for i := range policies.Items {
		if !keep["NetworkPolicy/"+policies.Items[i].Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &policies.Items[i])); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *OutgoingGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: outgoingReconcileKey, Namespace: layout.Namespace(layout.Edge)}}}
	})
	mine := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == layout.Namespace(layout.Edge) && obj.GetLabels()[egress.ManagedLabel] == "true"
	})
	class := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == netpolicy.OutgoingGatewayClassName
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("outgoing-gateway").
		// Every Component: its status is where a granted host is said. Of
		// its own objects it watches the two a person is likeliest to remove
		// by hand; the routes and policies are put back at the next change.
		Watches(&gentianov1alpha1.Component{}, one).
		Watches(&gatewayv1.GatewayClass{}, one, builder.WithPredicates(class)).
		Watches(&gatewayv1.Gateway{}, one, builder.WithPredicates(mine)).
		Watches(&corev1.Service{}, one, builder.WithPredicates(mine)).
		Complete(r)
}
