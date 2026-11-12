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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A token entry is the one gateway entry with no session in front: a host of
// its own on which a program presents a token the tenant's realm made out to
// that entry, and the bouncer verifies it (internal/bouncer, Route.Audience).
// The Gateway's sign-in filter is not on its route, so nothing there reads a
// cookie or sends anybody to sign in, and no other route of the component
// changes.

// ConditionTokenEntry is the Component condition that says whether the
// profile's token entry is served. Absent on a component that declares none.
const ConditionTokenEntry = "TokenEntry"

// tokenEntryRouteLabel marks the route of a token entry, so that it can be
// found and removed when the entry, or its approval, is gone.
const tokenEntryRouteLabel = "gentianos.io/token-entry"

// tokenEntryOf is the profile's token entry this component would route, or
// nil: a gateway entry with authMode bearer, to the component's own Service,
// of a profile of trustTier platform. The schema admits no other; this holds
// for a profile admitted before it did.
func tokenEntryOf(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.ExposureSpec {
	if profile.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform {
		return nil
	}
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.RequestKind() != gentianov1alpha1.ExposureKindTokenEntry {
			continue
		}
		if e.Backend.Component != "" && e.Backend.Component != comp.Name {
			continue
		}
		return e
	}
	return nil
}

// tokenEntryPolicyName is the SecurityPolicy of a component's token entry:
// one of its own, beside the policy that carries the session of its other
// routes.
func tokenEntryPolicyName(component string) string { return "sp-" + component + "-token" }

// tokenEntryRouteName is the route of a component's token entry.
func tokenEntryRouteName(component, entry string) string { return component + "-" + entry }

// tokenEntryPolicySpec puts the bouncer, and nothing else, in front of the
// route. No oidc block: the sign-in filter would answer a program with a
// redirect and replace the token it sent. Fail closed, as on every route.
func tokenEntryPolicySpec(route, edgeNamespace, bouncerService string) map[string]interface{} {
	backend := map[string]interface{}{"name": bouncerService, "port": int64(bouncerPort)}
	if edgeNamespace != "" {
		backend["namespace"] = edgeNamespace
	}
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{"group": gatewayv1.GroupName, "kind": "HTTPRoute", "name": route},
		},
		"extAuth": map[string]interface{}{
			"failOpen": false,
			"grpc":     map[string]interface{}{"backendRef": backend},
		},
	}
}

// tokenEntryMetadataName is the route, and the filter that answers it, of
// the document that says where a token for the entry is had.
func tokenEntryMetadataName(component, entry string) string {
	return component + "-" + entry + "-metadata"
}

// tokenEntryMetadata is the entry's Protected Resource Metadata (RFC 9728):
// the address tokens are made out to and the realm that issues them. It is
// what the bouncer's 401 points a program at, and it says nothing a person
// who knows the tenant's name could not work out.
func tokenEntryMetadata(host, kernelDomain, realm string) (string, error) {
	doc, err := json.Marshal(map[string]interface{}{
		"resource":                 tokenEntryAudience(host),
		"authorization_servers":    []string{fmt.Sprintf("https://id.%s/auth/realms/%s", kernelDomain, realm)},
		"bearer_methods_supported": []string{"header"},
	})
	return string(doc), err
}

// tokenEntryMetadataFilterSpec answers with the document, from the Gateway:
// no backend is asked, so the component behind the entry is not reachable
// without a token on this path either.
func tokenEntryMetadataFilterSpec(doc string) map[string]interface{} {
	return map[string]interface{}{
		"directResponse": map[string]interface{}{
			"statusCode":  int64(200),
			"contentType": "application/json",
			"body":        map[string]interface{}{"type": "Inline", "inline": doc},
			"header": map[string]interface{}{
				"set": []interface{}{
					map[string]interface{}{"name": "Cache-Control", "value": "no-store"},
				},
			},
		},
	}
}

// buildTokenEntryMetadataRoute is the one route of the entry's host that no
// policy names and the bouncer has no line for: GET of one exact path,
// answered by the filter and sent nowhere. An exact match wins over the
// entry's own prefix, so the request never reaches the entry's route.
func buildTokenEntryMetadataRoute(comp *gentianov1alpha1.Component, tenantName, name, host string, zone edgeZone, kernelDomain string) *gatewayv1.HTTPRoute {
	parent := gatewayParentRef(AuthenticatedGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	parent.Namespace = &ns
	section := gatewayv1.SectionName(zone.listenerFor(host, kernelDomain))
	parent.SectionName = &section
	match := pathMatch(gatewayv1.PathMatchExact, tokenEntryMetadataPath)
	get := gatewayv1.HTTPMethodGet
	match.Method = &get
	labels := componentLabels(comp, tenantName)
	labels[tokenEntryRouteLabel] = "true"
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: comp.Namespace, Labels: labels},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{match},
				Filters: []gatewayv1.HTTPRouteFilter{{
					Type: gatewayv1.HTTPRouteFilterExtensionRef,
					ExtensionRef: &gatewayv1.LocalObjectReference{
						Group: gatewayv1.Group(httpRouteFilterGVK.Group),
						Kind:  gatewayv1.Kind(httpRouteFilterGVK.Kind),
						Name:  gatewayv1.ObjectName(name),
					},
				}},
			}},
		},
	}
}

// tokenEntryApproved says whether the tenant's perimeter approver approved
// the entry as a token entry, and the approval has not expired.
func tokenEntryApproved(comp *gentianov1alpha1.Component, e *gentianov1alpha1.ExposureSpec, now time.Time) bool {
	if e == nil {
		return false
	}
	for i := range comp.Spec.Exposures {
		on := &comp.Spec.Exposures[i]
		if on.ExposureName != e.Name || !e.ApprovedAs(on.Kind) {
			continue
		}
		if on.ExpiresAt != nil && !now.Before(on.ExpiresAt.Time) {
			continue
		}
		return true
	}
	return false
}

// tokenEntryApprovalWaived says whether the entry is served without an
// approval: the tenant says so, and the cluster's claim lets tenants say so.
// The tenant's word alone waives nothing.
func tokenEntryApprovalWaived(tenant *gentianov1alpha1.Tenant, clusterAllows bool) bool {
	return clusterAllows && tenant != nil && tenant.Spec.TokenEntries != nil &&
		tenant.Spec.TokenEntries.Approval == gentianov1alpha1.TokenEntryApprovalWaived
}

// servedTokenEntry says whether the component's token entry is served, and
// writes the condition that says so (in memory: the caller's status update
// carries it). It is served when the tenant's perimeter approver approved the
// entry as a token entry and the approval has not expired, or the tenant
// waives the approval as the cluster allows (waived), and the entry's host is
// no other entry's.
func servedTokenEntry(
	comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
	e *gentianov1alpha1.ExposureSpec, host string, sessionHosts map[string]bool, waived bool, now time.Time,
) bool {
	if e == nil {
		apimeta.RemoveStatusCondition(&comp.Status.Conditions, ConditionTokenEntry)
		return false
	}
	approved := tokenEntryApproved(comp, e, now)
	byWaiver := !approved && waived
	approved = approved || waived
	cond := metav1.Condition{
		Type: ConditionTokenEntry, Status: metav1.ConditionTrue, Reason: "Approved",
		Message: fmt.Sprintf(
			"%s takes a token of the tenant's realm made out to https://%s in place of a session: approved by the tenant's perimeter approver. The right to use the component is still required",
			e.Name, host),
		ObservedGeneration: comp.Generation,
	}
	if byWaiver {
		cond.Reason = "ApprovalWaived"
		cond.Message = fmt.Sprintf(
			"%s takes a token of the tenant's realm made out to https://%s in place of a session. Nobody approved it: the tenant waives the approval of its token entries, as the cluster allows. The right to use the component is still required",
			e.Name, host)
	}
	switch {
	case host == "":
		cond.Status, cond.Reason = metav1.ConditionFalse, "NoHost"
		cond.Message = fmt.Sprintf("%s of profile %s has no host in this tenant's zone and is not served", e.Name, profile.Name)
	case sessionHosts[host]:
		cond.Status, cond.Reason = metav1.ConditionFalse, "HostShared"
		cond.Message = fmt.Sprintf(
			"%s of profile %s would answer on %s, where an entry behind sign-in already answers. A host takes a token or has a session, never both: the token entry is not served",
			e.Name, profile.Name, host)
	case !approved:
		cond.Status, cond.Reason = metav1.ConditionFalse, "AwaitingApproval"
		cond.Message = fmt.Sprintf(
			"%s of profile %s asks to take a token in place of a session, and the tenant's perimeter approver has not approved that (or the approval has expired). "+
				"Until then the entry has no route and answers nobody. "+
				"The request is listed with the tenant's public addresses (kubectl gentian exposures requests)",
			e.Name, profile.Name)
	}
	apimeta.SetStatusCondition(&comp.Status.Conditions, cond)
	return cond.Status == metav1.ConditionTrue
}

// ensureTokenEntry keeps the route and the policy of the component's token
// entry while it is served, and removes both when it is not: the entry gone
// from the profile, the approval withdrawn or expired.
//
// The policy is written before the route and removed after it. A route on
// the authenticated Gateway that no policy names is open, so there is no
// moment at which this one exists without the bouncer in front.
func (r *ComponentReconciler) ensureTokenEntry(
	ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant,
	profile *gentianov1alpha1.ComponentProfile, zone edgeZone, e *gentianov1alpha1.ExposureSpec, served, held bool,
) error {
	keep, metadata := "", ""
	if served {
		keep = tokenEntryRouteName(comp.Name, e.Name)
		metadata = tokenEntryMetadataName(comp.Name, e.Name)
		if err := r.ensureZoneGrant(ctx, comp, tenant.Name); err != nil {
			return err
		}
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(securityPolicyGVK)
		policy.SetName(tokenEntryPolicyName(comp.Name))
		policy.SetNamespace(comp.Namespace)
		policy.SetLabels(componentLabels(comp, tenant.Name))
		if err := unstructured.SetNestedField(policy.Object, tokenEntryPolicySpec(keep, servicesNamespace, r.bouncerService()), "spec"); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(comp, policy, r.Scheme); err != nil {
			return err
		}
		if err := ensureSpec(ctx, r.Client, policy); err != nil {
			return err
		}
		host := exposureHost(zone, comp, e)
		authz := exposureAuthz(tenant, comp, profile, r.Cluster, false)
		route := buildExposureRoute(comp, tenant.Name, keep, host, zone, e, authz, r.KernelDomain, nil)
		route.Labels[tokenEntryRouteLabel] = "true"
		if err := controllerutil.SetControllerReference(comp, route, r.Scheme); err != nil {
			return err
		}
		if err := withRestoreHold(ctx, r.Client, held, route); err != nil {
			return err
		}
		if err := ensureHTTPRouteResource(ctx, r.Client, route); err != nil {
			return err
		}
		// Where a token is had, on the same host and without one. The filter
		// first: a route whose filter is missing answers 500.
		doc, err := tokenEntryMetadata(host, r.KernelDomain, zone.realm)
		if err != nil {
			return err
		}
		filter := &unstructured.Unstructured{}
		filter.SetGroupVersionKind(httpRouteFilterGVK)
		filter.SetName(metadata)
		filter.SetNamespace(comp.Namespace)
		filter.SetLabels(componentLabels(comp, tenant.Name))
		if err := unstructured.SetNestedField(filter.Object, tokenEntryMetadataFilterSpec(doc), "spec"); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(comp, filter, r.Scheme); err != nil {
			return err
		}
		if err := ensureSpec(ctx, r.Client, filter); err != nil {
			return err
		}
		metadataRoute := buildTokenEntryMetadataRoute(comp, tenant.Name, metadata, host, zone, r.KernelDomain)
		if err := controllerutil.SetControllerReference(comp, metadataRoute, r.Scheme); err != nil {
			return err
		}
		if err := ensureHTTPRouteResource(ctx, r.Client, metadataRoute); err != nil {
			return err
		}
	}
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace),
		client.MatchingLabels{componentLabel: comp.Name, managedByLabel: managedByValue, tokenEntryRouteLabel: "true"}); err != nil {
		return err
	}
	for i := range list.Items {
		if n := list.Items[i].Name; n == keep || n == metadata {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if served {
		return nil
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(securityPolicyGVK)
	policy.SetName(tokenEntryPolicyName(comp.Name))
	policy.SetNamespace(comp.Namespace)
	if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return err
	}
	return nil
}
