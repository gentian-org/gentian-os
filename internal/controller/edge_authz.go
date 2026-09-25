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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// The kernel zone at the edge (networking.md §4): one confidential client,
// one cookie on the kernel domain, every kernel-zone route behind the same
// session. The client and its Secret are the identity bootstrap's; the
// policies that use them are reconciled here.
const (
	// edgeKernelClientID is the kernel zone's OIDC client in the kernel realm.
	edgeKernelClientID = "gentian-edge-kernel"
	// edgeKernelSecretName is the Secret in the edge namespace holding that
	// client's secret under the key Envoy Gateway reads.
	edgeKernelSecretName = "edge-kernel-oidc"
	// The zone's cookies. Named, and the same on every policy, so that the
	// routes share one session; Envoy Gateway would otherwise suffix each
	// policy's cookies with a hash of its own and give every host its own.
	edgeKernelAccessTokenCookie = "gentian-kernel-access"
	edgeKernelIDTokenCookie     = "gentian-kernel-id"
	// edgeAuthzRoutesConfigMap is the route table the ext-auth shim reads:
	// per host, the relation a caller must hold, written beside the routes.
	edgeAuthzRoutesConfigMap = "edge-authz-routes"
	edgeAuthzRoutesKey       = "routes.yaml"
	edgeAuthzPort            = int32(9001)
	// edgeOAuth2Prefix is where Envoy Gateway's OIDC filter answers the code
	// flow's callback and the logout: a route behind a session carries it,
	// or the flow has nowhere to land.
	edgeOAuth2Prefix = "/oauth2/"
)

// routeAuthz is what a route's exposure says must hold at L2.
type routeAuthz struct {
	relation string
	object   string
	// keepClientToken leaves the caller's own Authorization header alone
	// WITHOUT the edge putting its token there. The Keycloak console needs
	// exactly this: it mints a token with its own code flow inside the page
	// and calls the Admin REST API with it, so stripping the header is a 401
	// and replacing it with the edge's is "Token issued for an application
	// that is not the admin console".
	keepClientToken bool
	// forwardToken makes the EDGE put its own access token on the request.
	//
	// Only the desktop declares it: it relays that token to the director
	// (AD-13). Anything else wanting its header untouched wants
	// keepClientToken instead.
	forwardToken bool
}

var securityPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "SecurityPolicy",
}

// kernelZoneReady reports whether the kernel zone's client secret exists. A
// route behind the zone session is not created before it: a SecurityPolicy
// naming a missing Secret is invalid, and an invalid policy leaves its route
// served with no policy at all -- so until the identity bootstrap has run,
// the kernel UIs have no route rather than an open one.
func (r *GatewayPlatformReconciler) kernelZoneReady(ctx context.Context) bool {
	return kernelZoneReadyWith(ctx, r.Client)
}

func kernelZoneReadyWith(ctx context.Context, c client.Reader) bool {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: edgeKernelSecretName, Namespace: servicesNamespace}, secret)
	return err == nil && len(secret.Data["client-secret"]) > 0
}

func kernelSecurityPolicyName(route string) string { return "sp-" + route }

// kernelSecurityPolicySpec is L1 and L2 for one kernel-zone route: the
// zone's session (OIDC against the kernel realm with the zone client,
// cookie on the kernel domain) and the ext-auth shim, which fails closed.
func kernelSecurityPolicySpec(kernelDomain, kernelRealm, route string, authz routeAuthz, shimService string) map[string]interface{} {
	zone := edgeZone{
		domain: kernelDomain, realm: kernelRealm, clientID: edgeKernelClientID, secretName: edgeKernelSecretName,
		cookie: edgeKernelAccessTokenCookie, idCookie: edgeKernelIDTokenCookie, kernel: true,
	}
	return zoneSecurityPolicySpec(kernelDomain, zone, route, authz, "", shimService)
}

// zoneSecurityPolicySpec is L1 and L2 for one route in a zone. The zone's
// client Secret and the shim live in the edge namespace; a policy elsewhere
// names that namespace and relies on the ReferenceGrant the component
// reconciler keeps there.
// zoneSecurityPolicySpec is the session-and-shim policy for one route. The
// client secret is named without a namespace: Envoy Gateway (1.2) reads an
// OIDC client secret only from the policy's own namespace, ReferenceGrant or
// not, so whoever writes a policy outside the edge puts the zone's secret
// beside it (ensureZoneSecret). The shim is reached across namespaces, which
// a backendRef may do under a grant.
func zoneSecurityPolicySpec(kernelDomain string, zone edgeZone, route string, authz routeAuthz, edgeNamespace, shimService string) map[string]interface{} {
	clientSecret := map[string]interface{}{"name": zone.secretName}
	backend := map[string]interface{}{"name": shimService, "port": int64(edgeAuthzPort)}
	if edgeNamespace != "" {
		backend["namespace"] = edgeNamespace
	}
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": gatewayv1.GroupName,
				"kind":  "HTTPRoute",
				"name":  route,
			},
		},
		"oidc": map[string]interface{}{
			"provider": map[string]interface{}{
				"issuer": fmt.Sprintf("https://id.%s/auth/realms/%s", kernelDomain, zone.realm),
			},
			"clientID":     zone.clientID,
			"clientSecret": clientSecret,
			"logoutPath":   "/oauth2/logout",
			"cookieDomain": zone.domain,
			"cookieNames": map[string]interface{}{
				"accessToken": zone.cookie,
				"idToken":     zone.idCookie,
			},
			"forwardAccessToken": authz.forwardToken,
			"scopes":             []interface{}{"openid", "profile", "email"},
			"refreshToken":       true,
		},
		"extAuth": map[string]interface{}{
			"failOpen": false,
			"grpc":     map[string]interface{}{"backendRef": backend},
		},
	}
}

func (r *GatewayPlatformReconciler) ensureKernelSecurityPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	var policySpec map[string]interface{}
	switch {
	case spec.authz != nil:
		policySpec = kernelSecurityPolicySpec(r.KernelDomain, r.kernelRealm(), spec.name, *spec.authz, r.edgeAuthzService())
	case spec.securityPolicy != nil:
		policySpec = cloneMap(spec.securityPolicy)
		policySpec["targetRefs"] = []interface{}{
			map[string]interface{}{"group": gatewayv1.GroupName, "kind": "HTTPRoute", "name": spec.name},
		}
	default:
		return nil
	}
	name := kernelSecurityPolicyName(spec.name)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(securityPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(servicesNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentKernel,
	})
	if err := unstructured.SetNestedField(desired.Object, policySpec, "spec"); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(securityPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, policySpec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *GatewayPlatformReconciler) deleteStaleKernelSecurityPolicies(ctx context.Context, expected map[string]struct{}) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: securityPolicyGVK.Group, Version: securityPolicyGVK.Version, Kind: "SecurityPolicyList"})
	if err := r.List(ctx, list,
		client.InNamespace(servicesNamespace),
		client.MatchingLabels{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if _, ok := expected[list.Items[i].GetName()]; ok {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// edgeAuthzRoute is one line of the shim's table (internal/edge/authz).
type edgeAuthzRoute struct {
	Host              string `json:"host"`
	Relation          string `json:"relation"`
	Object            string `json:"object"`
	AccessTokenCookie string `json:"accessTokenCookie,omitempty"`
	IDTokenCookie     string `json:"idTokenCookie,omitempty"`
	EndSessionURL     string `json:"endSessionURL,omitempty"`
	KeepClientToken   bool   `json:"keepClientToken,omitempty"`
	ForwardToken      bool   `json:"forwardToken,omitempty"`
	AuthMode          string `json:"authMode"`
	// DenyPaths are refused at L2 before identity is looked at. Unioned
	// across every exposure that shares the host.
	DenyPaths []string `json:"denyPaths,omitempty"`
}

// endSessionURL is the realm's OIDC logout endpoint.
//
// Built rather than discovered. Keycloak publishes it in the realm's
// well-known document, but the edge authorization service answers requests on
// the hot path and must not depend on reaching the identity provider to do
// so: a sign-out that waited on discovery would fail in exactly the situation
// where a person most wants to sign out, which is when the identity provider
// is unwell. The shape has been stable across every Keycloak major this
// platform has run, and it is served under /auth like the rest of the realm.
func endSessionURL(kernelDomain, realm string) string {
	if kernelDomain == "" || realm == "" {
		return ""
	}
	return fmt.Sprintf("https://id.%s/auth/realms/%s/protocol/openid-connect/logout", kernelDomain, realm)
}

// edgeAuthzRouteTable renders the shim's table from the routes that carry an
// L2 question. Sorted by host: one table for one state, however the specs
// were listed.
func edgeAuthzRouteTable(specs []kernelHTTPRouteSpec, extra []edgeAuthzRoute, kernelDomain, kernelRealm string) (string, error) {
	var routes []edgeAuthzRoute
	for _, s := range specs {
		if s.authz == nil || s.host == "" {
			continue
		}
		routes = append(routes, edgeAuthzRoute{
			Host: s.host, Relation: s.authz.relation, Object: s.authz.object,
			AccessTokenCookie: edgeKernelAccessTokenCookie, ForwardToken: s.authz.forwardToken,
			KeepClientToken: s.authz.keepClientToken,
			// What sign-out needs, written here because the operator knows
			// the issuer and the realm and the edge must not have to ask.
			IDTokenCookie: edgeKernelIDTokenCookie,
			EndSessionURL: endSessionURL(kernelDomain, kernelRealm),
			AuthMode:      "oidc",
		})
	}
	routes = append(routes, extra...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })
	b, err := yaml.Marshal(map[string]interface{}{"routes": routes})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *GatewayPlatformReconciler) ensureEdgeAuthzRouteTable(ctx context.Context, specs []kernelHTTPRouteSpec) error {
	extra, err := componentRouteTableEntries(ctx, r.Client)
	if err != nil {
		return err
	}
	table, err := edgeAuthzRouteTable(specs, extra, r.KernelDomain, r.kernelRealm())
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      edgeAuthzRoutesConfigMap,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentKernel,
			},
		},
		Data: map[string]string{edgeAuthzRoutesKey: table},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data[edgeAuthzRoutesKey] == table {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}

func (r *GatewayPlatformReconciler) kernelRealm() string {
	if r.KernelRealm == "" {
		return "kernel"
	}
	return r.KernelRealm
}

func (r *GatewayPlatformReconciler) edgeAuthzService() string {
	if r.EdgeAuthzService == "" {
		return "gentian-os-edge-authz"
	}
	return r.EdgeAuthzService
}

// componentRouteTableEntries are the questions of every component route: the
// component reconciler writes them on the route, and this is the one writer
// of the table the shim reads.
func componentRouteTableEntries(ctx context.Context, c client.Reader) ([]edgeAuthzRoute, error) {
	list := &gatewayv1.HTTPRouteList{}
	if err := c.List(ctx, list, client.MatchingLabels{edgeAuthzRouteLabel: "true"}); err != nil {
		return nil, err
	}
	byHost := map[string]*edgeAuthzRoute{}
	var order []string
	for i := range list.Items {
		route := &list.Items[i]
		if route.DeletionTimestamp != nil || len(route.Spec.Hostnames) == 0 {
			continue
		}
		ann := route.Annotations
		if ann[edgeAuthzRelationAnnotation] == "" || ann[edgeAuthzObjectAnnotation] == "" {
			continue
		}
		mode := ann[edgeAuthzAuthModeAnnotation]
		if mode == "" {
			mode = "oidc"
		}
		denied := splitDenyPaths(ann[edgeAuthzDenyPathsAnnotation])
		for _, h := range route.Spec.Hostnames {
			host := string(h)
			// A component's routes share its host and its question; the
			// token is forwarded to the host if any of them says so, and a
			// path denied by any of them is denied for the host, because
			// deny wins.
			if cur, ok := byHost[host]; ok {
				cur.ForwardToken = cur.ForwardToken || ann[edgeAuthzForwardAnnotation] == "true"
				cur.DenyPaths = mergeDenyPaths(cur.DenyPaths, denied)
				continue
			}
			byHost[host] = &edgeAuthzRoute{
				Host: host, Relation: ann[edgeAuthzRelationAnnotation], Object: ann[edgeAuthzObjectAnnotation],
				AccessTokenCookie: ann[edgeAuthzCookieAnnotation], ForwardToken: ann[edgeAuthzForwardAnnotation] == "true",
				AuthMode: mode, DenyPaths: denied,
			}
			order = append(order, host)
		}
	}
	out := make([]edgeAuthzRoute, 0, len(order))
	for _, h := range order {
		out = append(out, *byHost[h])
	}
	return out, nil
}

// splitDenyPaths reads the annotation the component reconciler writes.
func splitDenyPaths(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mergeDenyPaths unions two lists, keeping the first list's order so the
// table is stable across reconciles and does not churn the ConfigMap.
func mergeDenyPaths(have, add []string) []string {
	if len(add) == 0 {
		return have
	}
	seen := make(map[string]bool, len(have))
	for _, p := range have {
		seen[p] = true
	}
	for _, p := range add {
		if !seen[p] {
			have = append(have, p)
			seen[p] = true
		}
	}
	return have
}
