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
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The edge's hold on an app being restored.
//
// A restore replaces an app's data and then runs the profile's post-restore
// commands, which rewrite what the app remembers of where it ran, and rolls
// it. Until all of that, and the realm, are back, someone signing in can be
// given a second account by an app that matches people by issuer or address,
// or be served settings of the old place. So from before the restore touches
// an app until the restore has ended, every route of the app answers 503
// with Retry-After instead of passing the request on.
//
// Derived from the TenantRestore's status alone (TenantRestore.HoldsBack):
// a restore that ends, either way, or is deleted lifts it, and an operator
// that restarts finds the same answer. The routes keep their hosts, their
// matches, their policies and their annotations; only what a matching rule
// does changes, so the session, the bouncer and every other route on the
// Gateway are as they were. The rules the route would have are kept on it
// (restoreHoldAnnotation), which is what lets the hold be lifted by the first
// thing a reconcile does, before anything that could stop it.

const (
	// restoreHoldFilterName is the Envoy Gateway HTTPRouteFilter a held
	// route's rules point at, one in each namespace with a held route.
	restoreHoldFilterName = "restore-hold"
	// restoreHoldAnnotation carries, on a held route, the rules it has when
	// it is not held, as JSON.
	restoreHoldAnnotation = "gentianos.io/restore-held-rules"
	// restoreHoldRetryAfter is what a held route tells a client to wait, in
	// seconds.
	restoreHoldRetryAfter = "60"
	restoreHoldBody       = "This app is being restored and is back when the restore has finished.\n"
)

var httpRouteFilterGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "HTTPRouteFilter",
}

// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=httproutefilters,verbs=get;list;watch;create;update;patch

// restoreHoldFilterSpec is the answer of a held route.
func restoreHoldFilterSpec() map[string]interface{} {
	return map[string]interface{}{
		"directResponse": map[string]interface{}{
			"statusCode":  int64(503),
			"contentType": "text/plain; charset=utf-8",
			"body":        map[string]interface{}{"type": "Inline", "inline": restoreHoldBody},
			"header": map[string]interface{}{
				"set": []interface{}{
					map[string]interface{}{"name": "Retry-After", "value": restoreHoldRetryAfter},
					map[string]interface{}{"name": "Cache-Control", "value": "no-store"},
				},
			},
		},
	}
}

func buildRestoreHoldFilter(namespace string) *unstructured.Unstructured {
	f := &unstructured.Unstructured{}
	f.SetGroupVersionKind(httpRouteFilterGVK)
	f.SetName(restoreHoldFilterName)
	f.SetNamespace(namespace)
	f.SetLabels(map[string]string{managedByLabel: managedByValue})
	_ = unstructured.SetNestedField(f.Object, restoreHoldFilterSpec(), "spec")
	return f
}

// ensureRestoreHoldFilter keeps the hold's answer in a namespace. It is left
// there once made: it does nothing unless a route points at it, and goes with
// the namespace.
func ensureRestoreHoldFilter(ctx context.Context, c client.Client, namespace string) error {
	desired := buildRestoreHoldFilter(namespace)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(httpRouteFilterGVK)
	err := c.Get(ctx, client.ObjectKey{Name: restoreHoldFilterName, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Object["spec"] = desired.Object["spec"]
	return c.Patch(ctx, existing, patch)
}

// heldRules are a route's rules as the hold has them: the same matches, each
// answered by the hold's filter and sent nowhere.
func heldRules(rules []gatewayv1.HTTPRouteRule) []gatewayv1.HTTPRouteRule {
	held := make([]gatewayv1.HTTPRouteRule, 0, len(rules))
	for _, rule := range rules {
		held = append(held, gatewayv1.HTTPRouteRule{
			Name:    rule.Name,
			Matches: rule.Matches,
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterExtensionRef,
				ExtensionRef: &gatewayv1.LocalObjectReference{
					Group: gatewayv1.Group(httpRouteFilterGVK.Group),
					Kind:  gatewayv1.Kind(httpRouteFilterGVK.Kind),
					Name:  restoreHoldFilterName,
				},
			}},
		})
	}
	return held
}

// routeHeld reports whether a route is held.
func routeHeld(route *gatewayv1.HTTPRoute) bool {
	_, held := route.Annotations[restoreHoldAnnotation]
	return held
}

// holdRoute turns a route that is not held into a held one, keeping the
// rules it had. A held route is left as it is.
func holdRoute(route *gatewayv1.HTTPRoute) error {
	if routeHeld(route) {
		return nil
	}
	kept, err := json.Marshal(route.Spec.Rules)
	if err != nil {
		return err
	}
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[restoreHoldAnnotation] = string(kept)
	route.Spec.Rules = heldRules(route.Spec.Rules)
	return nil
}

// releaseRoute gives a held route back the rules it had. Kept rules that
// cannot be read are not guessed at: the route is left to the reconcile that
// writes it, which writes its rules whole.
func releaseRoute(route *gatewayv1.HTTPRoute) error {
	kept, held := route.Annotations[restoreHoldAnnotation]
	if !held {
		return nil
	}
	var rules []gatewayv1.HTTPRouteRule
	if err := json.Unmarshal([]byte(kept), &rules); err != nil {
		return fmt.Errorf("route %s/%s: the rules kept by the restore hold cannot be read: %w", route.Namespace, route.Name, err)
	}
	route.Spec.Rules = rules
	delete(route.Annotations, restoreHoldAnnotation)
	return nil
}

// restoresHolding lists the apps a namespace's restores hold back.
func restoresHolding(ctx context.Context, c client.Reader, namespace string) (map[string]bool, error) {
	list := &gentianov1alpha1.TenantRestoreList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for i := range list.Items {
		for _, app := range list.Items[i].Status.HeldBack {
			if list.Items[i].HoldsBack(app) {
				held[app] = true
			}
		}
	}
	return held, nil
}

// heldForRestore reports whether a restore holds the component's app back.
// The app is the profile the component installs, which is the name a restore
// knows it by.
func heldForRestore(ctx context.Context, c client.Reader, comp *gentianov1alpha1.Component) (bool, error) {
	held, err := restoresHolding(ctx, c, comp.Namespace)
	if err != nil {
		return false, fmt.Errorf("restores holding %s back: %w", comp.Name, err)
	}
	return held[comp.Spec.ProfileRef.Name], nil
}

// componentRoutes are the routes the component reconciler writes for a
// component: its entries and its sign-in sidecar's, in its namespace, and its
// published surfaces, in its tenant's DMZ.
func componentRoutes(ctx context.Context, c client.Reader, comp *gentianov1alpha1.Component, dmz string) ([]gatewayv1.HTTPRoute, error) {
	var out []gatewayv1.HTTPRoute
	for _, ns := range []string{comp.Namespace, dmz} {
		if ns == "" {
			continue
		}
		list := &gatewayv1.HTTPRouteList{}
		err := c.List(ctx, list, client.InNamespace(ns),
			client.MatchingLabels{componentLabel: comp.Name, managedByLabel: managedByValue})
		if apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			// No Gateway API here: nothing is routed to hold.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// applyRestoreHold brings the component's routes that exist to what the
// restores in its namespace say: held, or not. First in a reconcile, before
// anything that could end it early, so that no failure elsewhere in the
// component keeps a route held after its restore has ended.
func (r *ComponentReconciler) applyRestoreHold(ctx context.Context, comp *gentianov1alpha1.Component, dmz string) (bool, error) {
	held, err := heldForRestore(ctx, r.Client, comp)
	if err != nil {
		return false, err
	}
	routes, err := componentRoutes(ctx, r.Client, comp, dmz)
	if err != nil {
		return false, err
	}
	filtered := map[string]bool{}
	for i := range routes {
		route := &routes[i]
		if routeHeld(route) == held {
			continue
		}
		patch := client.MergeFrom(route.DeepCopy())
		if held {
			if !filtered[route.Namespace] {
				if err := ensureRestoreHoldFilter(ctx, r.Client, route.Namespace); err != nil {
					return false, fmt.Errorf("restore hold in %s: %w", route.Namespace, err)
				}
				filtered[route.Namespace] = true
			}
			if err := holdRoute(route); err != nil {
				return false, err
			}
		} else if err := releaseRoute(route); err != nil {
			return false, err
		}
		if err := r.Patch(ctx, route, patch); err != nil {
			return false, err
		}
	}
	return held, nil
}

// restoreHold applies the hold to the component's routes, in its namespace
// and in its tenant's DMZ, and reports whether the app is held.
func (r *ComponentReconciler) restoreHold(ctx context.Context, comp *gentianov1alpha1.Component) (bool, error) {
	tenant, err := r.tenantOf(ctx, comp.Namespace)
	if err != nil {
		return false, err
	}
	dmz := ""
	if tenant != nil {
		dmz = layout.TenantDMZ(tenant.Name)
	}
	return r.applyRestoreHold(ctx, comp, dmz)
}

// withRestoreHold makes a route the component reconciler is about to write
// held when its app is: built whole as it would be, then held, so that the
// rules kept on it are the current ones. Not held, it is left as built, and
// writing it removes the annotation of an earlier hold.
func withRestoreHold(ctx context.Context, c client.Client, held bool, route *gatewayv1.HTTPRoute) error {
	if !held {
		return nil
	}
	if err := ensureRestoreHoldFilter(ctx, c, route.Namespace); err != nil {
		return fmt.Errorf("restore hold in %s: %w", route.Namespace, err)
	}
	return holdRoute(route)
}

// syncRestoreHoldAnnotation makes existing carry desired's hold annotation,
// or none, and reports whether it changed anything.
func syncRestoreHoldAnnotation(existing, desired *gatewayv1.HTTPRoute) bool {
	want, held := desired.Annotations[restoreHoldAnnotation]
	got, has := existing.Annotations[restoreHoldAnnotation]
	switch {
	case held && (!has || got != want):
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[restoreHoldAnnotation] = want
		return true
	case !held && has:
		delete(existing.Annotations, restoreHoldAnnotation)
		return true
	}
	return false
}

// restoreHoldInPlace reports whether every route of the app the component
// reconciler has written is held, and names those that are not. An app with
// no routes is unreachable already.
func restoreHoldInPlace(ctx context.Context, c client.Reader, tenant *gentianov1alpha1.Tenant, namespace, app string) (bool, string, error) {
	comps := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, comps, client.InNamespace(namespace)); err != nil {
		return false, "", err
	}
	var open []string
	for i := range comps.Items {
		comp := &comps.Items[i]
		if comp.Spec.ProfileRef.Name != app {
			continue
		}
		routes, err := componentRoutes(ctx, c, comp, layout.TenantDMZ(tenant.Name))
		if err != nil {
			return false, "", err
		}
		for j := range routes {
			if !routeHeld(&routes[j]) {
				open = append(open, routes[j].Namespace+"/"+routes[j].Name)
			}
		}
	}
	sort.Strings(open)
	return len(open) == 0, strings.Join(open, ", "), nil
}

// componentsOfRestore wakes every component in a restore's namespace: which
// of them it holds back is the component reconciler's to work out.
func componentsOfRestore(c client.Reader) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		comps := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, comps, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(comps.Items))
		for i := range comps.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
		}
		return out
	})
}

// restoreHoldChanged passes a restore's events on only when what it holds
// back changes: a running restore writes its status every few seconds, and
// none of that is the edge's business.
func restoreHoldChanged() predicate.Funcs {
	holding := func(obj client.Object) []string {
		restore, ok := obj.(*gentianov1alpha1.TenantRestore)
		if !ok {
			return nil
		}
		var out []string
		for _, app := range restore.Status.HeldBack {
			if restore.HoldsBack(app) {
				out = append(out, app)
			}
		}
		sort.Strings(out)
		return out
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return len(holding(e.Object)) > 0 },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return strings.Join(holding(e.ObjectOld), ",") != strings.Join(holding(e.ObjectNew), ",")
		},
	}
}

const (
	// restoreHoldPoll is how often a restore looks whether the edge holds
	// an app back yet.
	restoreHoldPoll = 2 * time.Second
	// restoreHoldTimeout bounds that wait. Nothing of the app has been
	// changed while it lasts, so giving up costs the restore and not the app.
	restoreHoldTimeout = 2 * time.Minute
)

// holdBack names the app in the restore's heldBack and reports whether the
// edge holds it back: every route of it is held. failure is set when the edge
// has not done so within restoreHoldTimeout.
func (r *TenantRestoreReconciler) holdBack(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
) (held bool, failure string, err error) {
	if !slices.Contains(restore.Status.HeldBack, appName) {
		restore.Status.HeldBack = append(restore.Status.HeldBack, appName)
		appStatus(&restore.Status.Apps, appName).HeldBackAt = ptrNow()
		// Written before it is looked for: the edge reads it from here.
		if err := r.persist(ctx, restore); err != nil {
			return false, "", err
		}
	}
	entry := appStatus(&restore.Status.Apps, appName)
	inPlace, open, err := restoreHoldInPlace(ctx, r.Client, tenant, restore.Namespace, appName)
	if err != nil {
		return false, "", err
	}
	if inPlace {
		return true, "", nil
	}
	if entry.HeldBackAt != nil && time.Since(entry.HeldBackAt.Time) > restoreHoldTimeout {
		return false, fmt.Sprintf("the edge did not hold the app back within %s (still served: %s); nothing of it was changed",
			restoreHoldTimeout, open), nil
	}
	entry.Message = "waiting for the edge to hold the app back: " + open
	return false, "", nil
}
