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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// The sign-in sidecar: how a person gets into an app that can do neither OIDC
// nor SAML itself (requires.services.identity.sidecar).
//
// A small program stands beside the app, on the app's own host. It is a SAML
// service provider towards the tenant's realm: it sends the browser there,
// checks the signed answer, and then runs the app's handler -- code from the
// app's catalogue entry that makes a session in this app for the person the
// answer names. The person was signed in at the platform already, so the
// realm answers without asking, and no password exists for the app at all.
//
// Three writers serve a declaration, and this file is the one that decides:
//
//   - This file decides whether an install gets a sidecar, where it answers
//     and which handler it runs. It writes the sidecar itself -- its handler,
//     what it is handed of the app's, its Deployment and Service, its network
//     paths -- and its routes, all owned by the Component, so that removing
//     the app removes them. The operator writes them rather than the
//     Composition because it already may: the provider that applies a
//     Composition's objects is not allowed to create a Deployment or a
//     NetworkPolicy anywhere, and is not made able to for this.
//   - The app Composition (app-default.yaml) registers the sidecar at the
//     tenant's realm, from where the app answers, which this file puts on the
//     App claim. The claim alone composes nothing, and neither does the
//     profile's declaration alone.
//   - The sidecar checks the answer (gentian-apps,
//     images/gentian-sidecar-sso-saml).
//
// What reaches the sidecar through the Gateway is two paths of the app's
// host, and they are treated differently on purpose:
//
//   - /sso/login is a rule of the app's own route. It is behind the zone's
//     session and the bouncer like every other path of the app, so only a
//     person who may use the app begins a sign-in, and the sidecar is told
//     who that is by the bouncer's identity headers.
//   - /sso/acs is a route of its own with NO policy: no session, no bouncer.
//     The realm posts its answer there from its own address. For a tenant on
//     a domain of its own that is another site, and a browser sends the
//     session's SameSite=Lax cookies with no such request -- the Gateway
//     would answer it with a redirect to the sign-in and the answer would be
//     lost. The path matches exactly, takes POST only, and reaches nothing
//     but the sidecar, which believes nothing about the request except a
//     response signed by the realm, addressed to it, answering a request it
//     sent for that browser and that person, once.
//
// Nothing else of the app's route changes: every other path keeps its session
// and its question.
//
// A third path reaches the sidecar and not through the Gateway at all:
//
//   - /sso/logout is where the realm tells the sidecar that a person signed
//     out, so that the session the handler made in the app ends then and not
//     an hour later. The realm posts a signed SAML LogoutRequest, server to
//     server, to the sidecar's own Service inside the cluster
//     (signInLogoutURL). No route carries the path, so nothing outside the
//     cluster reaches it; inside, the tenant's baseline admits the realm's
//     namespace to the tenant's pods (internal/kernel/netpolicy) and no pod
//     of the tenant to another. The sidecar answers the path only under its
//     Service's name and believes nothing but a request the realm signed,
//     addressed to exactly that address, once. What it does with one is the
//     app's handler's: a handler that can end a session does, and one that
//     cannot leaves the app's session to its hour.

const (
	// signInLoginPath begins a sign-in. Behind the session.
	signInLoginPath = "/sso/login"
	// signInACSPath is where the realm posts its answer. No session.
	signInACSPath = "/sso/acs"
	// signInLogoutPath is where the realm tells the sidecar that a person
	// signed out. It is on no route: the realm calls the sidecar's Service
	// inside the cluster.
	signInLogoutPath = "/sso/logout"
	// signInSidecarPort is the port the sidecar listens on, and its
	// Service's.
	signInSidecarPort = 8081
	// signInHandlerAsset is the asset label of the bundle's ConfigMap that
	// holds the handler, and what follows the profile's name in its name.
	signInHandlerAsset = "sign-in-handler"
	// signInHandlerKey is the key of the handler in that ConfigMap.
	signInHandlerKey = "handler.js"
	// signInRouteLabel marks the route that takes no session, so that it can
	// be found, and removed when a profile stops declaring a sidecar.
	signInRouteLabel = "gentianos.io/sign-in-route"

	reasonSignInSidecarRefused = "SignInSidecarRefused"
)

// The front door's identity headers (internal/bouncer). On the route that
// takes no session nothing sets them, so the Gateway removes whatever a
// client sent under these names before the sidecar is shown the request.
var frontDoorIdentityHeaders = bouncer.IdentityHeaders()

// signInSidecar is one install's sidecar, as decided here.
type signInSidecar struct {
	// exposure is the gateway entry whose host the two paths are on.
	exposure *gentianov1alpha1.ExposureSpec
	host     string
	// service is the sidecar's Service in the component's namespace, and the
	// name of its Deployment.
	service    string
	entryPaths []string
	// handler is the handler's source, from the bundle the install is pinned
	// to, and handlerDigest its sha256.
	handler       string
	handlerDigest string
	// realm is the tenant's realm; identityProvider is that realm as
	// browsers reach it and as its answers name it; descriptorURL is its
	// SAML descriptor inside the cluster, for its signing certificate.
	realm            string
	identityProvider string
	descriptorURL    string
	// What the handler is handed, each only where the profile declared it:
	// the app's own database (the server's namespace and port), the names
	// of the app's own secrets, and the app itself (where it is, and the
	// port its pods listen on).
	database          bool
	databaseNamespace string
	databasePort      int32
	secrets           []string
	appURL            string
	appPort           int32
	// logoutURL is where the realm tells the sidecar that a person signed
	// out: the sidecar's own Service inside the cluster.
	logoutURL string
	// claim is what the App claim carries for the Composition.
	claim map[string]interface{}
}

// signInSidecarService is the name of an app's sidecar Service, and of its
// Deployment.
func signInSidecarService(component string) string { return component + "-sign-in" }

// signInLogoutURL is where the realm tells an app's sidecar that a person
// signed out: the sidecar's Service, by its full name inside the cluster.
//
// The sidecar is given this string and the realm is registered with the same
// one, built by the Composition from the same parts (app-default.yaml: the
// claim's name, its namespace and the port the claim carries). The two have
// to agree to the letter -- the sidecar refuses a request addressed anywhere
// else -- and TestTheSidecarAndTheRealmAgreeOnTheSignOutAddress holds them
// to it.
func signInLogoutURL(component, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s",
		signInSidecarService(component), namespace, signInSidecarPort, signInLogoutPath)
}

// signInSidecarLabel marks everything that is part of one app's sidecar, and
// is what its Service and its network paths select its pod by. It is not the
// app's own label (gentianos.io/app): the sidecar is not one of the app's
// pods, and none of the app's network paths are its as well.
const signInSidecarLabel = "gentianos.io/sign-in-sidecar"

func signInSidecarLabels(comp *gentianov1alpha1.Component) map[string]string {
	labels := componentLabels(comp)
	labels[signInSidecarLabel] = comp.Name
	labels["app.kubernetes.io/name"] = "sign-in-sidecar"
	return labels
}

// signInACSRouteName is the route that carries the realm's answer.
func signInACSRouteName(component, exposure string) string {
	return component + "-" + exposure + "-sso-acs"
}

// declaredSignInSidecar is a profile's declaration, or nil.
func declaredSignInSidecar(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.SignInSidecarSpec {
	if s := profile.Services(); s != nil && s.Identity != nil {
		return s.Identity.Sidecar
	}
	return nil
}

// signInHandler is the handler a profile's bundle brings and its sha256, or
// "" when it brings none.
//
// Read from the bundle the profile carries, not from the ConfigMap the
// catalogue applied: those are the bytes the install's digest was taken over,
// and the component reconciler has compared the cluster with them before this
// is reached. The sidecar is given this source and told this digest, and
// loads no file that does not match it.
func signInHandler(profile *gentianov1alpha1.ComponentProfile) (source, digest string, err error) {
	bundle, err := profilebundle.Carried(profile)
	if err != nil {
		return "", "", err
	}
	if bundle == nil {
		return "", "", nil
	}
	want := profile.Name + "." + signInHandlerAsset
	for _, companion := range bundle.Companions {
		if companion.Kind != profilebundle.KindConfigMap || companion.Name != want {
			continue
		}
		data, _ := companion.Object["data"].(map[string]any)
		source, _ := data[signInHandlerKey].(string)
		if strings.TrimSpace(source) == "" {
			return "", "", nil
		}
		sum := sha256.Sum256([]byte(source))
		return source, hex.EncodeToString(sum[:]), nil
	}
	return "", "", nil
}

// signInExposure is the gateway entry a sidecar stands on: the one the
// declaration names, or the profile's only entry behind a session.
func signInExposure(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, name string) (*gentianov1alpha1.ExposureSpec, string) {
	var found []*gentianov1alpha1.ExposureSpec
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		own := e.Backend.Component == "" || e.Backend.Component == comp.Name
		session := e.Surface == gentianov1alpha1.SurfaceGateway && e.AuthMode == gentianov1alpha1.AuthModeOIDC
		if name != "" {
			if e.Name != name {
				continue
			}
			if !own || !session || e.Apex {
				return nil, fmt.Sprintf("identity.sidecar.exposure names %q, which is not a gateway entry of this component behind a session (surface gateway, authMode oidc)", name)
			}
			return e, ""
		}
		if own && session && !e.Apex {
			found = append(found, e)
		}
	}
	switch {
	case name != "":
		return nil, fmt.Sprintf("identity.sidecar.exposure names %q, and the profile has no such entry", name)
	case len(found) == 1:
		return found[0], ""
	case len(found) == 0:
		return nil, "identity.sidecar needs a gateway entry behind a session (surface gateway, authMode oidc) to stand on, and the profile has none"
	}
	return nil, "the profile has several gateway entries behind a session: identity.sidecar.exposure must name the one people open the app at"
}

// signInSidecarFor decides an install's sidecar: nil when the profile declares
// none, or a refusal that holds the install.
//
// A handler is handed what the declaration names -- the app's database login,
// its signing key -- so the rules are about which code that may be:
//
//   - Only from a catalogue of the whole cluster. A tenant's own catalogue
//     publishes profiles alone, so it has no handler to bring; and a profile
//     of such a catalogue that declares a sidecar is refused outright rather
//     than installed with no way in.
//   - Only for an install pinned to a digest, whose bundle brings the
//     handler. The component reconciler has by then compared the profile and
//     every companion on the cluster with that bundle
//     (profilebundle.VerifyOnCluster), and the sidecar is told the handler's
//     own digest and loads no other file.
//   - Never in the kernel realm: the realm is not the tenant's to register a
//     client in.
func (r *ComponentReconciler) signInSidecarFor(
	comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant, zone edgeZone,
) (*signInSidecar, string) {
	declared := declaredSignInSidecar(profile)
	if declared == nil {
		return nil, ""
	}
	refuse := func(format string, args ...interface{}) (*signInSidecar, string) {
		return nil, fmt.Sprintf("ComponentProfile %q declares requires.services.identity.sidecar: ", profile.Name) + fmt.Sprintf(format, args...)
	}
	origin, err := profilebundle.ParseOrigin(profile.Annotations[profilebundle.OriginAnnotation])
	if err != nil || origin.Source == "" || origin.Tenant != "" {
		return refuse("a sign-in handler is handed the app's own secrets, so it is run only from a catalogue of the whole cluster, and this profile is not from one")
	}
	if comp.Spec.ProfileRef.Digest == "" {
		return refuse("the install is not pinned to a digest, so nothing says which handler was reviewed; install it with its digest")
	}
	handler, digest, err := signInHandler(profile)
	if err != nil {
		return refuse("its bundle cannot be read: %v", err)
	}
	if digest == "" {
		return refuse("its bundle brings no handler (ConfigMap %s.%s with the key %s)", profile.Name, signInHandlerAsset, signInHandlerKey)
	}
	if tenantAdoptsKernelRealm(tenant, r.kernelRealm()) {
		return refuse("tenant %s signs in in the kernel realm, where no app registers a client", tenant.Name)
	}
	exposure, problem := signInExposure(comp, profile, declared.Exposure)
	if exposure == nil {
		return refuse("%s", problem)
	}
	host := exposureHost(zone, comp, exposure)
	if host == "" || r.KernelDomain == "" {
		return refuse("where entry %q answers is not known", exposure.Name)
	}
	for _, p := range declared.EntryPaths {
		if p == "/sso" || strings.HasPrefix(p, "/sso/") || strings.HasPrefix(p+"/", edgeOAuth2Prefix) {
			return refuse("entryPaths names %s, which is the sign-in's own", p)
		}
	}
	generated := map[string]bool{}
	for _, s := range profile.GeneratedSecrets() {
		generated[s.Name] = true
	}
	for _, name := range declared.Secrets {
		if !generated[name] {
			return refuse("secrets names %q, which is not under spec.secrets.generated: a handler is given this app's own secrets only", name)
		}
	}
	if declared.Database && (profile.Services() == nil || profile.Services().Database == nil) {
		return refuse("database is set and the profile declares no requires.services.database")
	}

	realm := keycloakRealmName(tenant)
	sidecar := &signInSidecar{
		exposure:      exposure,
		host:          host,
		service:       signInSidecarService(comp.Name),
		entryPaths:    declared.EntryPaths,
		handler:       handler,
		handlerDigest: digest,
		realm:         realm,
		// The realm, as browsers reach it and as its answers name it. Every
		// realm of every tenant is at the cluster's identity provider,
		// whatever the tenant's own domain is.
		identityProvider: fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, realm),
		// The realm's signing certificate, read inside the cluster.
		descriptorURL: fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/auth/realms/%s/protocol/saml/descriptor",
			suzeKeycloakHTTPServiceName(), identityNamespace, realm),
		secrets:   declared.Secrets,
		logoutURL: signInLogoutURL(comp.Name, comp.Namespace),
		// Where the app answers. The sidecar's name at the realm and the one
		// address the realm may post to follow from it, here and in the
		// Composition: https://<host>/sso and https://<host>/sso/acs.
		//
		// And the port of the sidecar's Service on which it is told of a
		// sign-out. The Composition builds the rest of that address itself,
		// from the claim's own name and namespace, so the realm can be told
		// to call nothing but this app's sidecar.
		claim: map[string]interface{}{"host": host, "logoutPort": int64(signInSidecarPort)},
	}
	// A handler that may call the app is told where it is: the Service and
	// port the entry routes to, inside the cluster. The network path is to
	// the declared port of the app's own pods.
	if declared.AppPort > 0 {
		sidecar.appPort = declared.AppPort
		sidecar.appURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
			exposure.Backend.Service, comp.Namespace, exposure.Backend.Port)
	}
	// The database server of the engine the profile declared, and its port:
	// the same server and port the app's own pods are opened to
	// (internal/kernel/netpolicy), and no other.
	if declared.Database {
		sidecar.database = true
		switch provisioner.DatabaseEngineOf(profile) {
		case gentianov1alpha1.DatabaseEnginePostgreSQL:
			sidecar.databaseNamespace, sidecar.databasePort = layout.System("postgresql"), provisioner.PostgresPort
		case gentianov1alpha1.DatabaseEngineMariaDB:
			sidecar.databaseNamespace, sidecar.databasePort = layout.System("mariadb"), provisioner.MariaDBPort
		default:
			return refuse("database is set and the profile's database engine is not one the platform serves")
		}
	}
	return sidecar, ""
}

// signInRouteRules are the rules a sidecar adds to its entry's own route:
// the login path to the sidecar, and each entry path to the login path. They
// are part of that route, so the route's session and its question apply.
func signInRouteRules(namespace string, s *signInSidecar, filters ...gatewayv1.HTTPRouteFilter) []gatewayv1.HTTPRouteRule {
	rules := []gatewayv1.HTTPRouteRule{
		kernelBackendRuleNS(s.service, namespace, signInSidecarPort, pathMatch(gatewayv1.PathMatchExact, signInLoginPath), filters...),
	}
	for _, p := range s.entryPaths {
		rules = append(rules, signInEntryRule(p))
	}
	return rules
}

// signInEntryRule sends a page load of one of the app's paths to the login
// path. GET only: anything else is a request the app's own page made, and a
// redirect would hand it a sign-in page for an answer.
func signInEntryRule(path string) gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	target := signInLoginPath
	get := gatewayv1.HTTPMethodGet
	match := pathMatch(gatewayv1.PathMatchExact, path)
	match.Method = &get
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{match},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme:     &scheme,
				Port:       &port,
				StatusCode: &status,
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: &target,
				},
			},
		}},
	}
}

// buildSignInACSRoute is the one route of an app that takes no session: the
// path the realm posts its answer to, exactly, by POST, to the sidecar.
//
// It carries no bouncer label and no question, so it has no line in the
// bouncer's table, and no SecurityPolicy names it. The identity headers are
// removed here because on this route nothing else would: a client could send
// them, and the bouncer that overwrites them is not asked.
//
// No frame policy either. The sidecar answers this path with a redirect, or
// with a page that carries a policy of its own, and the route's would replace
// that one.
func buildSignInACSRoute(comp *gentianov1alpha1.Component, zone edgeZone, s *signInSidecar, kernelDomain string) *gatewayv1.HTTPRoute {
	parent := gatewayParentRef(AuthenticatedGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	parent.Namespace = &ns
	section := gatewayv1.SectionName(zone.listenerFor(s.host, kernelDomain))
	parent.SectionName = &section

	post := gatewayv1.HTTPMethodPost
	match := pathMatch(gatewayv1.PathMatchExact, signInACSPath)
	match.Method = &post
	rule := kernelBackendRuleNS(s.service, comp.Namespace, signInSidecarPort, match, gatewayv1.HTTPRouteFilter{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Remove: frontDoorIdentityHeaders},
	})

	labels := componentLabels(comp)
	labels[signInRouteLabel] = "acs"
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      signInACSRouteName(comp.Name, s.exposure.Name),
			Namespace: comp.Namespace,
			Labels:    labels,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(s.host)},
			Rules:           []gatewayv1.HTTPRouteRule{rule},
		},
	}
}

// ensureSignInACSRoute keeps the session-less route of a component's sidecar,
// and removes any such route the component no longer has a sidecar for: one
// whose profile stopped declaring it, or whose entry was renamed.
func (r *ComponentReconciler) ensureSignInACSRoute(ctx context.Context, comp *gentianov1alpha1.Component, zone edgeZone, s *signInSidecar) error {
	keep := ""
	if s != nil {
		route := buildSignInACSRoute(comp, zone, s, r.KernelDomain)
		if err := controllerutil.SetControllerReference(comp, route, r.Scheme); err != nil {
			return err
		}
		if err := ensureHTTPRouteResource(ctx, r.Client, route); err != nil {
			return err
		}
		keep = route.Name
		if err := r.ensureSignInACSRateLimit(ctx, comp, route); err != nil {
			return err
		}
	}
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace),
		client.MatchingLabels{componentLabel: comp.Name, managedByLabel: managedByValue, signInRouteLabel: "acs"}); err != nil {
		return err
	}
	for i := range list.Items {
		if list.Items[i].Name == keep {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.deleteSignInACSRateLimit(ctx, comp.Namespace, list.Items[i].Name); err != nil {
			return err
		}
	}
	return nil
}

// ensureSignInACSRateLimit holds the route that takes no session to one
// client address's allowance of posts (edge_rate_limit.go). The route is the
// one place of an app where a signed answer is checked for anybody who sends
// one, so it is the place to bound how fast anybody can.
//
// A BackendTrafficPolicy of the route's own name, owned by the Component
// like the route. With the limit switched off there is none, and one an
// earlier setting left is removed.
func (r *ComponentReconciler) ensureSignInACSRateLimit(ctx context.Context, comp *gentianov1alpha1.Component, route *gatewayv1.HTTPRoute) error {
	limit := edgeSignInRateLimit("", edgeClientAddressHeader(ctx, r.Client))
	if limit == nil {
		return r.deleteSignInACSRateLimit(ctx, route.Namespace, route.Name)
	}
	spec := map[string]interface{}{
		"targetRefs": []interface{}{map[string]interface{}{
			"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route.Name,
		}},
		"rateLimit": limit,
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(backendTrafficPolicyGVK)
	desired.SetName(route.Name)
	desired.SetNamespace(route.Namespace)
	desired.SetLabels(route.Labels)
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(backendTrafficPolicyGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
		return err
	}
	return r.Patch(ctx, existing, patch)
}

func (r *ComponentReconciler) deleteSignInACSRateLimit(ctx context.Context, namespace, name string) error {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(backendTrafficPolicyGVK)
	policy.SetName(name)
	policy.SetNamespace(namespace)
	err := r.Delete(ctx, policy)
	if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// ensureSignInSidecar keeps an app's sidecar, and removes one the app no
// longer has: the handler, what the handler is handed, the Deployment, the
// Service and the network paths. Each is owned by the Component, so an
// uninstall takes them with it; a profile that stops declaring the sidecar
// loses them here.
func (r *ComponentReconciler) ensureSignInSidecar(
	ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant, s *signInSidecar, pull pullSecrets,
) error {
	if s == nil {
		return r.removeSignInSidecar(ctx, comp)
	}
	labels := signInSidecarLabels(comp)
	owned := func(obj client.Object) error { return controllerutil.SetControllerReference(comp, obj, r.Scheme) }

	// The handler: the reviewed file, and nothing the cluster holds under
	// its name.
	handler := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: s.service + "-handler", Namespace: comp.Namespace, Labels: labels},
		Data:       map[string]string{signInHandlerKey: s.handler},
	}
	if err := owned(handler); err != nil {
		return err
	}
	if err := upsert(ctx, r.Client, handler, func(existing *corev1.ConfigMap) bool {
		if equality.Semantic.DeepEqual(existing.Data, handler.Data) && equality.Semantic.DeepEqual(existing.Labels, handler.Labels) && ownedBy(existing, comp) {
			return false
		}
		existing.Data, existing.Labels, existing.OwnerReferences = handler.Data, handler.Labels, handler.OwnerReferences
		return true
	}); err != nil {
		return err
	}

	handed, err := r.ensureSignInSecret(ctx, comp, tenant, s, labels)
	if err != nil {
		return err
	}

	deploy := signInDeployment(comp, s, labels, handed, pull)
	if err := owned(deploy); err != nil {
		return err
	}
	if err := upsert(ctx, r.Client, deploy, func(existing *appsv1.Deployment) bool {
		if equality.Semantic.DeepEqual(existing.Spec, deploy.Spec) && ownedBy(existing, comp) {
			return false
		}
		existing.Spec, existing.Labels, existing.OwnerReferences = deploy.Spec, deploy.Labels, deploy.OwnerReferences
		return true
	}); err != nil {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.service, Namespace: comp.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{signInSidecarLabel: comp.Name},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: signInSidecarPort,
				TargetPort: intstr.FromInt32(signInSidecarPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	if err := owned(svc); err != nil {
		return err
	}
	if err := upsert(ctx, r.Client, svc, func(existing *corev1.Service) bool {
		if equality.Semantic.DeepEqual(existing.Spec.Ports, svc.Spec.Ports) &&
			equality.Semantic.DeepEqual(existing.Spec.Selector, svc.Spec.Selector) && ownedBy(existing, comp) {
			return false
		}
		existing.Spec.Ports, existing.Spec.Selector, existing.OwnerReferences = svc.Spec.Ports, svc.Spec.Selector, svc.OwnerReferences
		return true
	}); err != nil {
		return err
	}

	return r.ensureSignInPolicies(ctx, comp, s, labels)
}

// ensureSignInSecret keeps what the handler is handed: the Secret
// "<app>-sign-in", filled by the secrets operator from this app's own vault
// paths. It answers whether there is one.
//
// The paths are built here from the tenant and the app, and a profile names
// only which of the app's secrets it wants: no name it could state reaches
// another app's path. Nothing is handed over that was not declared, and the
// operator reads none of it -- it names where the values are.
func (r *ComponentReconciler) ensureSignInSecret(
	ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant, s *signInSidecar, labels map[string]string,
) (bool, error) {
	var data []interface{}
	add := func(key, path, property string) {
		data = append(data, map[string]interface{}{
			"secretKey": key,
			"remoteRef": map[string]interface{}{"key": path, "property": property},
		})
	}
	if s.database {
		path := secrets.CategoryPath(tenant.Name, comp.Name, "database")
		for _, property := range []string{"host", "port", "name", "user", "password"} {
			add("DB_"+strings.ToUpper(property), path, property)
		}
	}
	for _, name := range s.secrets {
		add("SECRET_"+strings.ToUpper(name), secrets.InternalPath(tenant.Name, comp.Name, name), "value")
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, types.NamespacedName{Name: s.service, Namespace: comp.Namespace}, existing)
	if len(data) == 0 {
		if err == nil {
			return false, client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return false, client.IgnoreNotFound(err)
	}
	desired := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": externalSecretGVK.GroupVersion().String(),
		"kind":       externalSecretGVK.Kind,
		"metadata":   map[string]interface{}{"name": s.service, "namespace": comp.Namespace},
		"spec": map[string]interface{}{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]interface{}{"name": "openbao", "kind": "ClusterSecretStore"},
			// The Secret is this object's and goes with it.
			"target": map[string]interface{}{"name": s.service, "creationPolicy": "Owner", "deletionPolicy": "Delete"},
			"data":   data,
		},
	}}
	desired.SetLabels(labels)
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return false, err
	}
	switch {
	case apierrors.IsNotFound(err):
		return true, r.Create(ctx, desired)
	case err != nil:
		return false, err
	}
	// Only on a real difference: the secrets operator writes this object's
	// status, and an unconditional update would race it.
	if equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) &&
		equality.Semantic.DeepEqual(existing.GetLabels(), desired.GetLabels()) && ownedBy(existing, comp) {
		return true, nil
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	return true, r.Update(ctx, desired)
}

// signInDeployment is the sidecar's pod: one container, read-only, no
// service account token, everything dropped, running as nobody in
// particular.
//
// One replica, replaced and never doubled: a sign-in under way is remembered
// in the process that began it, and an answer that arrived at another process
// would be refused.
func signInDeployment(comp *gentianov1alpha1.Component, s *signInSidecar, labels map[string]string, handed bool, pull pullSecrets) *appsv1.Deployment {
	replicas := int32(1)
	no, yes := false, true
	user := int64(1000) // the image's own unprivileged user
	env := []corev1.EnvVar{
		{Name: "SSO_ENTITY_ID", Value: "https://" + s.host + "/sso"},
		{Name: "SSO_ACS_URL", Value: "https://" + s.host + signInACSPath},
		{Name: "SSO_LOGIN_PATH", Value: signInLoginPath},
		{Name: "SSO_LOGOUT_URL", Value: s.logoutURL},
		{Name: "SSO_IDP_ENTITY_ID", Value: s.identityProvider},
		{Name: "SSO_IDP_SSO_URL", Value: s.identityProvider + "/protocol/saml"},
		{Name: "SSO_IDP_DESCRIPTOR_URL", Value: s.descriptorURL},
		{Name: "SSO_REALM", Value: s.realm},
		{Name: "SSO_HANDLER_SHA256", Value: s.handlerDigest},
	}
	if s.appURL != "" {
		env = append(env, corev1.EnvVar{Name: "APP_URL", Value: s.appURL})
	}
	container := corev1.Container{
		Name:            "sign-in",
		Image:           kernel.SignInSidecarImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{{
			Name: "http", ContainerPort: signInSidecarPort, Protocol: corev1.ProtocolTCP,
		}},
		Env: env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &no,
			ReadOnlyRootFilesystem:   &yes,
			RunAsNonRoot:             &yes,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "handler", MountPath: "/usr/src/app/custom", ReadOnly: true},
		},
		// Ready is "the realm's signing certificate is known": until then
		// every answer would be refused, so the route has nothing behind it.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(signInSidecarPort)}},
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(signInSidecarPort)}},
			PeriodSeconds: 30,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("192Mi"),
			},
		},
	}
	if handed {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: s.service}},
		}}
	}
	var pullRefs []corev1.LocalObjectReference
	for _, name := range pull.images {
		pullRefs = append(pullRefs, corev1.LocalObjectReference{Name: name})
	}
	selector := map[string]string{signInSidecarLabel: comp.Name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.service, Namespace: comp.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// A handler is loaded once, when the process starts.
					// Naming it here is what starts a new process for a
					// new handler.
					Annotations: map[string]string{"gentianos.io/sign-in-handler": s.handlerDigest},
				},
				Spec: corev1.PodSpec{
					// Nothing here calls the API server.
					AutomountServiceAccountToken: &no,
					EnableServiceLinks:           &no,
					ImagePullSecrets:             pullRefs,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						RunAsUser:      &user,
						RunAsGroup:     &user,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name: "handler", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: s.service + "-handler"},
						}},
					}},
				},
			},
		},
	}
}

// ensureSignInPolicies bound the sidecar's network paths.
//
// Out: the realm's signing certificate, on the identity provider's HTTP port;
// and what the profile declared for the handler, each on one port -- the
// app's database server, the app's own pods. The sidecar does not carry the
// app's label, so none of the app's own paths are its as well. (The name
// service is every pod's, from the tenant's baseline.)
//
// In, to the sidecar: nothing is written. The tenant's baseline admits the
// edge to every pod of the tenant and no pod of the tenant to another, which
// is what the sidecar wants: it is reached through the Gateway and by nothing
// that could claim to be it. The same baseline admits the identity
// provider's namespace to every pod of the tenant, which is the path the
// realm tells the sidecar of a sign-out on; a rule here could add nothing to
// that and, policies being additive, could take nothing from it either.
//
// In, to the app: its pods admit the sidecar on the declared port, where a
// handler may call the app. The baseline closes the namespace, so without
// this the call is refused.
func (r *ComponentReconciler) ensureSignInPolicies(
	ctx context.Context, comp *gentianov1alpha1.Component, s *signInSidecar, labels map[string]string,
) error {
	tcp := corev1.ProtocolTCP
	port := func(p int32) []networkingv1.NetworkPolicyPort {
		value := intstr.FromInt32(p)
		return []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &value}}
	}
	namespace := func(name string) []networkingv1.NetworkPolicyPeer {
		return []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": name},
		}}}
	}
	sidecarPods := metav1.LabelSelector{MatchLabels: map[string]string{signInSidecarLabel: comp.Name}}
	appPods := metav1.LabelSelector{MatchLabels: map[string]string{meta.AppLabel: comp.Name}}

	rules := []networkingv1.NetworkPolicyEgressRule{
		{To: namespace(identityNamespace), Ports: port(identityProviderHTTPPort)},
	}
	if s.database {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: namespace(s.databaseNamespace), Ports: port(s.databasePort)})
	}
	if s.appPort > 0 {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &appPods}}, Ports: port(s.appPort),
		})
	}
	egress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: s.service, Namespace: comp.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: sidecarPods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}
	if err := controllerutil.SetControllerReference(comp, egress, r.Scheme); err != nil {
		return err
	}
	if err := upsert(ctx, r.Client, egress, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, egress.Spec) && ownedBy(existing, comp) {
			return false
		}
		existing.Spec, existing.Labels, existing.OwnerReferences = egress.Spec, egress.Labels, egress.OwnerReferences
		return true
	}); err != nil {
		return err
	}

	toApp := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: s.service + "-to-app", Namespace: comp.Namespace, Labels: labels},
	}
	if s.appPort == 0 {
		return client.IgnoreNotFound(r.Delete(ctx, toApp))
	}
	toApp.Spec = networkingv1.NetworkPolicySpec{
		PodSelector: appPods,
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &sidecarPods}},
			Ports: port(s.appPort),
		}},
	}
	if err := controllerutil.SetControllerReference(comp, toApp, r.Scheme); err != nil {
		return err
	}
	return upsert(ctx, r.Client, toApp, func(existing *networkingv1.NetworkPolicy) bool {
		if equality.Semantic.DeepEqual(existing.Spec, toApp.Spec) && ownedBy(existing, comp) {
			return false
		}
		existing.Spec, existing.Labels, existing.OwnerReferences = toApp.Spec, toApp.Labels, toApp.OwnerReferences
		return true
	})
}

// identityProviderHTTPPort is the port the identity provider's pods listen
// on, which is what a network policy matches: the same number its Service
// serves, and the one the descriptor's address names.
const identityProviderHTTPPort = 8080

// removeSignInSidecar deletes whatever of a sidecar an app still has. Found
// by the sidecar's label and this component's own, so nothing of another
// app's is touched, and nothing of the app itself.
//
// Asked on every pass of a component that has none, which is every app but a
// few; each is a list from the cache.
func (r *ComponentReconciler) removeSignInSidecar(ctx context.Context, comp *gentianov1alpha1.Component) error {
	selector := client.MatchingLabels{signInSidecarLabel: comp.Name, componentLabel: comp.Name, managedByLabel: managedByValue}
	in := client.InNamespace(comp.Namespace)

	// A kind this process does not know, or the cluster does not have, has
	// no object of the sidecar's to remove.
	absent := func(err error) bool {
		return err != nil && (apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err))
	}
	secretsList := &unstructured.UnstructuredList{}
	secretsList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: externalSecretGVK.Group, Version: externalSecretGVK.Version, Kind: externalSecretGVK.Kind + "List",
	})
	lists := []client.ObjectList{
		secretsList, &appsv1.DeploymentList{}, &corev1.ServiceList{}, &corev1.ConfigMapList{}, &networkingv1.NetworkPolicyList{},
	}
	for _, list := range lists {
		if err := r.List(ctx, list, in, selector); err != nil {
			if absent(err) {
				continue
			}
			return err
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			return err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}
