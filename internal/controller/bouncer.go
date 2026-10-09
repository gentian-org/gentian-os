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
	"regexp"
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
	// bouncerRoutesConfigMap is the route table the ext-auth bouncer reads:
	// per host, the relation a caller must hold, written beside the routes.
	bouncerRoutesConfigMap = "bouncer-routes"
	bouncerRoutesKey       = "routes.yaml"
	bouncerPort            = int32(9001)
	// edgeOAuth2Prefix is where Envoy Gateway's OAuth2 filter answers the
	// code flow's callback and the logout: a route behind a session carries
	// it, or the flow has nowhere to land.
	edgeOAuth2Prefix = "/oauth2/"
	// edgeLogoutPath is the one path that signs a person out: the filter
	// drops the host's cookies and sends the browser to the realm's
	// end-session endpoint, which it reads from the issuer's discovery
	// document, with the session's ID token as the hint and the host's front
	// page as the way back.
	edgeLogoutPath = "/oauth2/logout"
	// edgeIDTokenHeader is where the filter hands the session's ID token to
	// the bouncer on a route that keeps the caller's own Authorization
	// header (internal/bouncer, HeaderIDToken: the two must agree). The
	// filter removes whatever a client sent under this name before it sets
	// its own.
	edgeIDTokenHeader = "x-gentian-id-token"
)

// edgeFilterCookiePrefixes are the cookies Envoy Gateway's OAuth2 filter
// names itself, each a fixed word, a hyphen and a suffix.
//
// The suffix is Envoy Gateway's: a hash of the SecurityPolicy's UID, which
// exists only once the policy does and changes when the policy is made
// again, while a browser still holds the cookies of the one before. The two
// that carry a sign-in in progress get a further suffix per sign-in. So the
// words are stated here and the bouncer takes out whatever begins with one,
// rather than the operator reading the UID back and stating names that would
// miss the cookies an earlier policy left behind.
//
// AccessToken- and IdToken- are what the filter would call the two token
// cookies if a policy did not name them. Every zone policy does name them
// (cookieNames); they are listed so that a policy which lost its names would
// not start handing tokens to backends.
var edgeFilterCookiePrefixes = []string{
	"AccessToken-", "IdToken-", "RefreshToken-",
	"OauthHMAC-", "OauthExpires-", "OauthNonce-", "CodeVerifier-",
}

// sessionCookies are the two cookies a zone's policy names: the session's
// access token and its ID token. The same names go into the policy
// (zoneSecurityPolicySpec) and into the bouncer's table.
func (z edgeZone) sessionCookies() []string {
	var out []string
	for _, n := range []string{z.cookie, z.idCookie} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

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
	//
	// The bouncer still has to be shown the session, and on such a route it
	// cannot be shown it in the Authorization header. The filter forwards
	// the session's ID token in a header of its own instead.
	keepClientToken bool
	// forwardToken lets the edge's access token go on to the BACKEND.
	//
	// The filter puts that token on every request of an ordinary route, for
	// the bouncer; the bouncer removes it again unless the route says this.
	// Only the desktop declares it: it relays that token to the director.
	// Anything else wanting its header untouched wants keepClientToken
	// instead.
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
// cookie on the kernel domain) and the ext-auth bouncer, which fails closed.
func kernelSecurityPolicySpec(kernelDomain, kernelRealm, route string, authz routeAuthz, bouncerService string) map[string]interface{} {
	zone := edgeZone{
		realm: kernelRealm, clientID: edgeKernelClientID, secretName: edgeKernelSecretName,
		cookie: edgeKernelAccessTokenCookie, idCookie: edgeKernelIDTokenCookie,
	}
	return zoneSecurityPolicySpec(kernelDomain, zone, route, authz, "", bouncerService)
}

// zoneSecurityPolicySpec is the session-and-bouncer policy for one route: L1,
// the zone's session, and L2, the bouncer.
//
// The order the two run in is not this policy's to say. Envoy Gateway puts
// ext_authz ahead of its OAuth2 filter unless the EnvoyProxy says otherwise,
// and this platform's says otherwise (filterOrder in the edge EnvoyProxy):
// the OAuth2 filter first, the bouncer after it. Everything below is written
// for that order.
//
//   - The filter answers its own callback and logout paths, sends a request
//     with no session to sign in, and refreshes a session whose access token
//     has run out before the request goes any further. None of those reach
//     the bouncer.
//   - What does reach the bouncer carries the session's current token, put
//     there by the filter: the access token in the Authorization header
//     (forwardAccessToken), which makes the filter remove whatever the client
//     sent in that header first; or, where the header is the page's own
//     (keepClientToken), the ID token in a header the filter likewise clears
//     before setting.
//   - The bouncer verifies that token itself and refuses a request without
//     one. There is no pass-through: passThroughAuthHeader and denyRedirect
//     are not set, and must not be without the bouncer's table saying what
//     such a request may reach.
//
// The token cookies stay encrypted, which is the filter's default. Nothing
// but the filter reads them.
//
// The client secret is named without a namespace, so it is read from the
// policy's own: whoever writes a policy outside the edge puts the zone's
// secret beside it (ensureZoneSecret). The bouncer is reached across
// namespaces, which a backendRef may do under a grant.
func zoneSecurityPolicySpec(kernelDomain string, zone edgeZone, route string, authz routeAuthz, edgeNamespace, bouncerService string) map[string]interface{} {
	clientSecret := map[string]interface{}{"name": zone.secretName}
	backend := map[string]interface{}{"name": bouncerService, "port": int64(bouncerPort)}
	if edgeNamespace != "" {
		backend["namespace"] = edgeNamespace
	}
	oidc := map[string]interface{}{
		"provider": map[string]interface{}{
			"issuer": fmt.Sprintf("https://id.%s/auth/realms/%s", kernelDomain, zone.realm),
		},
		"clientID":     zone.clientID,
		"clientSecret": clientSecret,
		"logoutPath":   edgeLogoutPath,
		// No cookieDomain, so the session cookie is scoped to the host that
		// set it.
		//
		// It used to be scoped to the whole zone, .<kernel>, so one sign-in
		// covered every host in it -- which also meant the browser sent that
		// cookie to every application in the zone. An application that is
		// compromised, or merely careless about what it logs, saw a
		// credential good for every other application beside it.
		//
		// The cost is one silent round trip to Keycloak the first time a
		// browser reaches each host, because the Keycloak session already
		// exists and the redirect comes straight back. Single sign-on is
		// preserved and so is signing out everywhere at once: the realm
		// session ends, and no host's cookie can be refreshed against it.
		"cookieNames": map[string]interface{}{
			"accessToken": zone.cookie,
			"idToken":     zone.idCookie,
		},
		// Lax: the cookies go with a navigation to the host and with
		// anything the host's own pages ask for, and with nothing a page on
		// another site makes the browser send, short of a link somebody
		// follows. One value covers every cookie the filter sets, the ones
		// that carry the code flow among them; those come back on a
		// top-level redirect from the realm, which Lax allows.
		"cookieConfig": map[string]interface{}{"sameSite": "Lax"},
		// The bouncer's token. Not what reaches the backend: the bouncer
		// removes the header again unless the route forwards it.
		"forwardAccessToken": !authz.keepClientToken,
		"scopes":             []interface{}{"openid", "profile", "email"},
		"refreshToken":       true,
	}
	if authz.keepClientToken {
		oidc["forwardIDToken"] = map[string]interface{}{"header": edgeIDTokenHeader}
	}
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": gatewayv1.GroupName,
				"kind":  "HTTPRoute",
				"name":  route,
			},
		},
		"oidc": oidc,
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
		policySpec = kernelSecurityPolicySpec(r.KernelDomain, r.kernelRealm(), spec.name, *spec.authz, r.bouncerService())
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

// bouncerRoute is one line of the bouncer's table (internal/bouncer).
type bouncerRoute struct {
	Host            string `json:"host"`
	Relation        string `json:"relation"`
	Object          string `json:"object"`
	KeepClientToken bool   `json:"keepClientToken,omitempty"`
	// IDTokenAudience is the zone's client, on a route that keeps the
	// caller's own token: whose ID token proves the session there.
	IDTokenAudience string `json:"idTokenAudience,omitempty"`
	ForwardToken    bool   `json:"forwardToken,omitempty"`
	// ExchangeScope makes the backend's token one the realm issued for this
	// app alone (internal/bouncer, exchange.go).
	ExchangeScope string `json:"exchangeScope,omitempty"`
	AuthMode      string `json:"authMode"`
	// DenyPaths are refused at L2 before identity is looked at. Unioned
	// across every exposure that shares the host.
	DenyPaths []string `json:"denyPaths,omitempty"`
	// SessionCookies and SessionCookiePrefixes are the cookies the edge
	// keeps the session in on this host. The Gateway's filter decrypts the
	// token cookies into the request it passes on; the bouncer takes these
	// out again, so that no backend is handed a token in its Cookie header.
	// On every route with a session, and on no other.
	SessionCookies        []string `json:"sessionCookies,omitempty"`
	SessionCookiePrefixes []string `json:"sessionCookiePrefixes,omitempty"`
}

// bouncerRouteTable renders the bouncer's table from the routes that carry an
// L2 question. Sorted by host: one table for one state, however the specs
// were listed.
//
// Every route in it needs a session. A route that needs none has no policy
// and never asks the bouncer; there is no entry that says "let this through".
func bouncerRouteTable(specs []kernelHTTPRouteSpec, extra []bouncerRoute) (string, error) {
	return bouncerTable(specs, extra, nil)
}

// bouncerTable is the whole table: the routes, and the components that hold
// a key for the one question the bouncer answers apart from the edge
// (rights_check.go). A table with no checker says nothing about them.
func bouncerTable(specs []kernelHTTPRouteSpec, extra []bouncerRoute, checkers []bouncerChecker) (string, error) {
	var routes []bouncerRoute
	for _, s := range specs {
		if s.authz == nil || s.host == "" {
			continue
		}
		route := bouncerRoute{
			Host: s.host, Relation: s.authz.relation, Object: s.authz.object,
			ForwardToken:    s.authz.forwardToken,
			KeepClientToken: s.authz.keepClientToken,
			AuthMode:        "oidc",
			// A kernel route's session is the kernel zone's.
			SessionCookies:        []string{edgeKernelAccessTokenCookie, edgeKernelIDTokenCookie},
			SessionCookiePrefixes: edgeFilterCookiePrefixes,
		}
		if s.authz.keepClientToken {
			route.IDTokenAudience = edgeKernelClientID
		}
		routes = append(routes, route)
	}
	routes = append(routes, extra...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })
	table := map[string]interface{}{"routes": routes}
	if len(checkers) > 0 {
		table["checkers"] = checkers
	}
	b, err := yaml.Marshal(table)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *GatewayPlatformReconciler) ensureBouncerRouteTable(ctx context.Context, specs []kernelHTTPRouteSpec) error {
	extra, err := componentRouteTableEntries(ctx, r.Client)
	if err != nil {
		return err
	}
	checkers, err := rightsCheckers(ctx, r.Client)
	if err != nil {
		return err
	}
	table, err := bouncerTable(specs, extra, checkers)
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bouncerRoutesConfigMap,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentKernel,
			},
		},
		Data: map[string]string{bouncerRoutesKey: table},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data[bouncerRoutesKey] == table {
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

func (r *GatewayPlatformReconciler) bouncerService() string {
	if r.BouncerService == "" {
		return "gentian-os-bouncer"
	}
	return r.BouncerService
}

// componentRouteTableEntries are the questions of every component route: the
// component reconciler writes them on the route, and this is the one writer
// of the table the bouncer reads.
func componentRouteTableEntries(ctx context.Context, c client.Reader) ([]bouncerRoute, error) {
	list := &gatewayv1.HTTPRouteList{}
	if err := c.List(ctx, list, client.MatchingLabels{bouncerRouteLabel: "true"}); err != nil {
		return nil, err
	}
	byHost := map[string]*bouncerRoute{}
	var order []string
	for i := range list.Items {
		route := &list.Items[i]
		if route.DeletionTimestamp != nil || len(route.Spec.Hostnames) == 0 {
			continue
		}
		ann := route.Annotations
		if ann[bouncerRelationAnnotation] == "" || ann[bouncerObjectAnnotation] == "" {
			continue
		}
		mode := ann[bouncerAuthModeAnnotation]
		if mode == "" {
			mode = "oidc"
		}
		denied := splitDenyPaths(ann[bouncerDenyPathsAnnotation])
		// Only a route with a session has session cookies. The names are
		// the zone's, which the component reconciler wrote on the route;
		// the filter's own are the same everywhere.
		var cookies, prefixes []string
		if mode == "oidc" {
			cookies = splitDenyPaths(ann[bouncerSessionCookiesAnnotation])
			prefixes = edgeFilterCookiePrefixes
		}
		for _, h := range route.Spec.Hostnames {
			host := string(h)
			// A component's routes share its host and its question; the
			// token is forwarded to the host if any of them says so, and a
			// path denied by any of them is denied for the host, because
			// deny wins.
			if cur, ok := byHost[host]; ok {
				cur.ForwardToken = cur.ForwardToken || ann[bouncerForwardAnnotation] == "true"
				if cur.ExchangeScope == "" {
					cur.ExchangeScope = ann[bouncerExchangeScopeAnnotation]
				}
				cur.DenyPaths = mergeDenyPaths(cur.DenyPaths, denied)
				cur.SessionCookies = mergeDenyPaths(cur.SessionCookies, cookies)
				if cur.SessionCookiePrefixes == nil {
					cur.SessionCookiePrefixes = prefixes
				}
				continue
			}
			byHost[host] = &bouncerRoute{
				Host: host, Relation: ann[bouncerRelationAnnotation], Object: ann[bouncerObjectAnnotation],
				ForwardToken:  ann[bouncerForwardAnnotation] == "true",
				ExchangeScope: ann[bouncerExchangeScopeAnnotation],
				AuthMode:      mode, DenyPaths: denied,
				SessionCookies: cookies, SessionCookiePrefixes: prefixes,
			}
			order = append(order, host)
		}
	}
	out := make([]bouncerRoute, 0, len(order))
	for _, h := range order {
		route := *byHost[h]
		// A host's backends are handed one token or the other, and the
		// bouncer refuses a table that says both. A profile cannot declare
		// both; should two routes of a host ever disagree, the exchange is
		// what goes, and the host behaves as it did before it was asked for.
		if route.ForwardToken || route.AuthMode != "oidc" {
			route.ExchangeScope = ""
		}
		out = append(out, route)
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

// The exchange of a session's token for an app's own (exposure
// exchangeToken) is asked of the tenant's realm by that realm's exchange
// client, and the bouncer is who asks. Each tenant's realm has its own
// client and its own secret, written by the tenant's Composition into a
// Secret of the edge namespace that carries the realm's name in a label. The
// bouncer is one program for all of them and mounts one Secret, so they are
// gathered into it here: one entry per realm, named after the realm.
const (
	// exchangeRealmLabel marks a Secret holding one realm's exchange client
	// secret, and its value is the realm.
	exchangeRealmLabel = "gentianos.io/edge-exchange-realm"
	// exchangeSecretKey is the entry of such a Secret that holds the secret.
	exchangeSecretKey = "client-secret"
	// bouncerExchangeSecret is the one Secret the bouncer mounts
	// (charts/gentian-os, bouncer.yaml: the two must agree).
	bouncerExchangeSecret = "bouncer-exchange"
)

// exchangeRealmName is what a realm may be called to become a file name in
// the bouncer's mount and a path of the token endpoint (internal/bouncer,
// realmNamePattern: the two must agree).
var exchangeRealmName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (r *GatewayPlatformReconciler) ensureBouncerExchangeSecrets(ctx context.Context) error {
	list := &corev1.SecretList{}
	if err := r.List(ctx, list, client.InNamespace(servicesNamespace), client.HasLabels{exchangeRealmLabel}); err != nil {
		return err
	}
	data := map[string][]byte{}
	for i := range list.Items {
		s := &list.Items[i]
		realm := s.Labels[exchangeRealmLabel]
		secret := s.Data[exchangeSecretKey]
		// The gathered Secret is itself in this namespace; it carries no
		// such label, and is skipped by name as well in case one is ever put
		// on it.
		if s.Name == bouncerExchangeSecret || s.DeletionTimestamp != nil || len(secret) == 0 || !exchangeRealmName.MatchString(realm) {
			continue
		}
		data[realm] = secret
	}
	key := types.NamespacedName{Name: bouncerExchangeSecret, Namespace: servicesNamespace}
	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		if len(data) == 0 {
			return nil
		}
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				Labels: map[string]string{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
	}
	if err != nil {
		return err
	}
	if existing.Labels[managedByLabel] != managedByValue {
		return fmt.Errorf("secret %s/%s exists and is not the operator's", key.Namespace, key.Name)
	}
	if equality.Semantic.DeepEqual(existing.Data, data) || (len(existing.Data) == 0 && len(data) == 0) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = data
	return r.Patch(ctx, existing, patch)
}
