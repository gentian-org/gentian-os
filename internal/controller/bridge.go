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
	"sort"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
	"github.com/gentian-org/gentian-os/internal/bouncer"
)

// An external bridge (api/v1alpha1/bridge_types.go) is served at the tenant's
// door: one host of the zone that takes a token of the tenant's realm in
// place of a session, on which every bridge of the tenant answers under the
// paths of its protocols and nothing else does.
//
// At the edge a bridge is a token entry (token_entry.go) that routes to the
// Service of the app it connects. It is approved, served, challenged and
// removed as one. What differs is said on its route for the bouncer: the
// question is the app's (may this person use it), the paths are the bridge's
// alone, and the app is handed a token of the person made out to it, never
// the one the caller presented at the door.

const (
	// BridgeDoorLabel is the host label of a tenant's door: <label>.<the
	// tenant's domain>. Kept from every app (internal/hostnames).
	BridgeDoorLabel = "sync"

	// bridgeEntryName names a bridge in the tenant's approvals, where a
	// token entry is named by its exposure: a bridge has one entry.
	bridgeEntryName = "bridge"

	// bouncerDoorPathsAnnotation makes a route that takes a token one app
	// of a door: the path prefixes it serves, comma-separated. The route's
	// relation, object and exchange scope are then asked under those
	// prefixes alone.
	bouncerDoorPathsAnnotation = "gentianos.io/bouncer-door-paths"
	// bouncerDoorObjectAnnotation is what the door itself is recorded as
	// for a request that reached no app: the tenant.
	bouncerDoorObjectAnnotation = "gentianos.io/bouncer-door-object"
	// doorRelation is the relation recorded with it. Asked of nobody.
	doorRelation = "can_enter"

	// ConditionBridge says whether a bridge's app and paths let it be
	// served. Absent on every other component.
	ConditionBridge = "Bridge"
)

// bridgeRedirectURIs are where a program on a person's own computer is sent
// back to after the person signed in: a loopback address, on any port.
var bridgeRedirectURIs = []string{"http://127.0.0.1/", "http://localhost/"}

// BridgeTokenScope is the client scope of a tenant's realm a token is
// exchanged for at the door before a bridged app sees a request: it names the
// app's own sign-in client as the audience and carries what that app's
// sign-in tokens carry of the person, and nothing else.
//
// Not AppTokenScope: that one names the profile, for an app that is handed a
// token because it has no sign-in of its own. An app behind a bridge verifies
// the token as it verifies its own.
func BridgeTokenScope(appProfile string) string { return "bridge-" + appProfile }

// isBridge reports a profile of class bridge that says what it connects.
func isBridge(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Spec.Bridge != nil &&
		classIncludes(profile, gentianov1alpha1.ComponentClassBridge)
}

// bridgePaths are the path prefixes a bridge answers under, over all its
// protocols, each once and in one order.
func bridgePaths(profile *gentianov1alpha1.ComponentProfile) []string {
	if !isBridge(profile) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range profile.Spec.Bridge.Protocols {
		for _, path := range p.Paths {
			path = strings.TrimSuffix(strings.TrimSpace(path), "/")
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// bridgeEntryOf is a served bridge as the entry the edge routes: on the
// tenant's door, taking a token, to the Service the bridge names. Nil for
// every component that is not an external bridge routed to a Service.
//
// The profile declares no such exposure and may not (a bridge exposes
// nothing of its own): the entry is the platform's reading of spec.bridge,
// so that what is approved, routed and held against a token is one thing
// with what any token entry is.
func bridgeEntryOf(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.ExposureSpec {
	if !isBridge(profile) || profile.Spec.Bridge.Kind != gentianov1alpha1.BridgeKindExternal {
		return nil
	}
	api := profile.Spec.Package.API
	paths := bridgePaths(profile)
	if api == nil || api.Backend == nil || len(paths) == 0 {
		return nil
	}
	return &gentianov1alpha1.ExposureSpec{
		Name:               bridgeEntryName,
		Surface:            gentianov1alpha1.SurfaceGateway,
		AuthMode:           gentianov1alpha1.AuthModeBearer,
		SubDomain:          BridgeDoorLabel,
		Paths:              paths,
		Backend:            gentianov1alpha1.BackendRef{Service: api.Backend.Service, Port: api.Backend.Port},
		ClientRedirectURIs: bridgeRedirectURIs,
	}
}

// bridgeDoor is what a bridge's route says to the bouncer beside what a
// token entry's says.
type bridgeDoor struct {
	// authz is the question a caller is asked: may this person use the app.
	authz routeAuthz
	// tenantObject is the door's own name in the record.
	tenantObject string
	// scope is what the caller's token is exchanged for.
	scope string
	// paths are the prefixes the bridge serves.
	paths []string
}

// bridgeDoorOf is the door a bridge's route describes.
func bridgeDoorOf(tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile) *bridgeDoor {
	if bridgeEntryOf(profile) == nil {
		return nil
	}
	app := profile.Spec.Bridge.App.Profile
	return &bridgeDoor{
		// The object the app's own routes and its tile ask on
		// (exposureAuthz): a bridge is never more open than the app.
		authz:        routeAuthz{relation: "can_use", object: "app:" + tenant.Name + "/" + app},
		tenantObject: "tenant:" + tenant.Name,
		scope:        BridgeTokenScope(app),
		paths:        bridgePaths(profile),
	}
}

// annotate writes the door on a route built as a token entry's.
func (d *bridgeDoor) annotate(route *gatewayv1.HTTPRoute) {
	if d == nil || route == nil {
		return
	}
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[bouncerRelationAnnotation] = d.authz.relation
	route.Annotations[bouncerObjectAnnotation] = d.authz.object
	route.Annotations[bouncerExchangeScopeAnnotation] = d.scope
	route.Annotations[bouncerDoorPathsAnnotation] = strings.Join(d.paths, ",")
	route.Annotations[bouncerDoorObjectAnnotation] = d.tenantObject
}

// pathsOverlap reports two prefixes one of which is under the other, on
// segment boundaries: a request under the longer is under both.
func pathsOverlap(a, b string) bool {
	a, b = strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/")
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// bridgeServable says whether a bridge can be served, apart from its
// approval, and writes the condition that says so (in memory: the caller's
// status update carries it). It can when the app it connects is installed in
// the tenant and none of its paths is under, or over, a path another
// component already serves at the door.
//
// A path is the first component's that served it. Two routes of one host
// that match one path are resolved by the Gateway by age and with no error
// anywhere, and the one that lost would answer nobody while saying Ready.
func (r *ComponentReconciler) bridgeServable(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, host string,
) (bool, error) {
	if bridgeEntryOf(profile) == nil {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionBridge)
		return false, nil
	}
	app := profile.Spec.Bridge.App.Profile
	cond := metav1.Condition{
		Type: ConditionBridge, Status: metav1.ConditionTrue, Reason: "Connected",
		Message:            fmt.Sprintf("connects %s under %s", app, strings.Join(bridgePaths(profile), ", ")),
		ObservedGeneration: comp.Generation,
	}
	installed, err := r.profileInstalled(ctx, comp.Namespace, app)
	if err != nil {
		return false, err
	}
	taken, by, err := r.doorPathTaken(ctx, comp, bridgePaths(profile), host)
	if err != nil {
		return false, err
	}
	switch {
	case !installed:
		cond.Status, cond.Reason = metav1.ConditionFalse, "AppNotInstalled"
		cond.Message = fmt.Sprintf("the app this bridge connects, %s, is not installed in the tenant: the bridge is not served until it is", app)
	case taken != "":
		cond.Status, cond.Reason = metav1.ConditionFalse, "PathTaken"
		cond.Message = fmt.Sprintf("%s is already served on %s by %s: a path at the door is one component's, and this bridge is not served", taken, host, by)
	}
	apimeta.SetStatusCondition(&comp.Status.Conditions, cond)
	return cond.Status == metav1.ConditionTrue, nil
}

// profileInstalled reports whether a component of the profile is installed
// in the namespace and not on its way out.
func (r *ComponentReconciler) profileInstalled(ctx context.Context, namespace, profile string) (bool, error) {
	list := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		c := &list.Items[i]
		if c.DeletionTimestamp == nil && c.Spec.ProfileRef.Name == profile {
			return true, nil
		}
	}
	return false, nil
}

// doorPathTaken is the first of the paths that another component's route
// already serves on the host, and that component, or "".
func (r *ComponentReconciler) doorPathTaken(
	ctx context.Context, comp *gentianov1alpha1.Component, paths []string, host string,
) (path, by string, err error) {
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace), client.MatchingLabels{bouncerRouteLabel: "true"}); err != nil {
		return "", "", err
	}
	for i := range list.Items {
		route := &list.Items[i]
		if route.DeletionTimestamp != nil || route.Labels[componentLabel] == comp.Name {
			continue
		}
		onHost := false
		for _, h := range route.Spec.Hostnames {
			onHost = onHost || string(h) == host
		}
		if !onHost {
			continue
		}
		theirs := splitDenyPaths(route.Annotations[bouncerDoorPathsAnnotation])
		for _, mine := range paths {
			// A route of the host that names no paths serves all of it.
			if len(theirs) == 0 {
				return mine, route.Labels[componentLabel], nil
			}
			for _, other := range theirs {
				if pathsOverlap(mine, other) {
					return mine, route.Labels[componentLabel], nil
				}
			}
		}
	}
	return "", "", nil
}

// ensureBridgeClient keeps the realm client programs sign people in through
// to reach a bridge, while the bridge is approved, and removes it when it is
// not: without the approval the realm issues nobody a token for the door in
// this bridge's name.
//
// The client's tokens are made out to the door, and name the edge's exchange
// client beside it: the bouncer exchanges one for a token of the app the
// bridge connects, and the realm exchanges a token only for a client the
// token names.
func (r *ComponentReconciler) ensureBridgeClient(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
	tenant *gentianov1alpha1.Tenant, zone edgeZone, pull pullSecrets, signIn *signInSidecar, operatorValues map[string]interface{},
) (ready bool, message string, err error) {
	e := bridgeEntryOf(profile)
	if e == nil {
		return true, "", nil
	}
	in, err := r.renderInput(ctx, comp, profile, tenant, zone, pull, false, signIn, operatorValues)
	if err != nil {
		return false, "", err
	}
	if tokenEntryApproved(comp, e, time.Now()) || tokenEntryApprovalWaived(tenant, clusterAllowsTokenEntryWaiver(ctx, r.Client)) {
		in.TokenEntry = &apprender.TokenEntry{
			Host:          exposureHost(zone, comp, e),
			RedirectURIs:  e.ClientRedirectURIs,
			Exchangers:    []string{bouncer.ExchangeClientID},
			SessionMaxAge: tokenEntrySessionMaxAge(tenant, clusterAllowsTokenEntrySessions(ctx, r.Client)),
		}
	}
	rendered, err := apprender.RenderTokenEntryClient(in)
	if err != nil {
		return false, "", err
	}
	ready, _, message, err = r.applyRendered(ctx, comp, tenant.Name, rendered)
	return ready, message, err
}
