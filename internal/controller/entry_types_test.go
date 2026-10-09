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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The two things an app's entry may ask a perimeter approver for beside a
// plain public address: behind sign-in, to keep its own Authorization header
// (clientAuthorization: app); and on a public address, to be passed its
// callers' Authorization header (authMode: app). Neither is in force before
// the approval, and neither approval does the other's work.

// flowsBundle is an app whose pages send a token of the app's own in the
// Authorization header: one host behind sign-in that asks to keep it, and a
// second host that asks for nothing.
const flowsBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: flows
spec:
  classes: [app]
  launch: from
  launchFrom: file-store
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/flows
      name: flows
      version: "1.0.0"
  expose:
  - name: web
    surface: gateway
    authMode: oidc
    clientAuthorization: app
    backend: {service: flows, port: 8080}
  - name: hooks
    surface: gateway
    authMode: oidc
    subDomain: hooks
    backend: {service: flows, port: 8080}
`

func flowsHarness(t *testing.T, bundle string) *digestHarness {
	t.Helper()
	h := startDigestHarness(t, materialised(t, bundle), "")
	zoneSecret := &corev1.Secret{Data: map[string][]byte{"client-secret": []byte("s")}}
	zoneSecret.Name, zoneSecret.Namespace = "edge-acme-oidc", servicesNamespace
	if err := h.c.Create(context.Background(), zoneSecret); err != nil {
		t.Fatal(err)
	}
	return h
}

// approve records an approval on the Component, as the tenant reconciler
// copies it from the registry.
func (h *digestHarness) approve(on ...gentianov1alpha1.ExposureEnablement) {
	h.t.Helper()
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), comp); err != nil {
		h.t.Fatal(err)
	}
	comp.Spec.Exposures = on
	if err := h.c.Update(context.Background(), comp); err != nil {
		h.t.Fatal(err)
	}
}

func (h *digestHarness) policy() map[string]interface{} {
	h.t.Helper()
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(securityPolicyGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "sp-" + h.comp.Name, Namespace: h.comp.Namespace}, policy); err != nil {
		h.t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(policy.Object, "spec")
	return spec
}

func (h *digestHarness) table() map[string]bouncerRoute {
	h.t.Helper()
	entries, err := componentRouteTableEntries(context.Background(), h.c)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bouncerRoute{}
	for _, e := range entries {
		out[e.Host] = e
	}
	return out
}

func keptApproval(entry string, expires *metav1.Time) gentianov1alpha1.ExposureEnablement {
	return gentianov1alpha1.ExposureEnablement{
		ExposureName: entry, Owner: "u-pat", Kind: gentianov1alpha1.ExposureKindSignInAppAuthorization,
		ReviewAt: metav1.NewTime(time.Now().Add(24 * time.Hour)), ExpiresAt: expires,
	}
}

// requiresTheSession holds the properties of a policy that make sign-in
// unavoidable on its routes, whatever a request carries in its Authorization
// header. They are the whole of what Envoy Gateway (the pinned release's
// translator, internal/xds/translator/oidc.go) reads to decide whether a
// request may pass its OAuth2 filter without the session's cookies:
// passThroughAuthHeader is the one setting that builds a pass-through
// matcher, and denyRedirect the one that answers instead of signing in.
// Leaving the client's header alone (forwardAccessToken false) is
// preserve_authorization_header on the filter, which keeps a header and
// admits nobody. Behind the filter the bouncer fails closed.
func requiresTheSession(t *testing.T, what string, spec map[string]interface{}) {
	t.Helper()
	oidc, ok := spec["oidc"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: the policy has no session at all: %v", what, spec)
	}
	for _, never := range []string{"passThroughAuthHeader", "denyRedirect"} {
		if v, set := oidc[never]; set {
			t.Fatalf("%s: oidc.%s = %v: a request could pass the session on what it carries", what, never, v)
		}
	}
	if _, set := spec["jwt"]; set {
		t.Fatalf("%s: the policy verifies a bearer of its own beside the session", what)
	}
	ext, _ := spec["extAuth"].(map[string]interface{})
	if ext == nil || ext["failOpen"] != false {
		t.Fatalf("%s: the bouncer is not asked, or fails open: %v", what, spec["extAuth"])
	}
	if oidc["clientID"] == "" || oidc["clientSecret"] == nil {
		t.Fatalf("%s: the session names no client: %v", what, oidc)
	}
}

// Before anybody approves it, an entry that asks to keep its own header is
// served exactly as an entry that asks for nothing: same policy, same table
// entry, and the Component says why the app's own calls fail.
func TestAnUnapprovedRequestToKeepTheHeaderChangesNothing(t *testing.T) {
	asks := flowsHarness(t, flowsBundle)
	plain := flowsHarness(t, strings.Replace(flowsBundle, "    clientAuthorization: app\n", "", 1))
	comp := asks.reconcile()
	plain.reconcile()

	if !reflect.DeepEqual(asks.policy(), plain.policy()) {
		t.Fatalf("an unapproved request changed the policy:\n%v\n%v", asks.policy(), plain.policy())
	}
	oidc := asks.policy()["oidc"].(map[string]interface{})
	if oidc["forwardAccessToken"] != true || oidc["forwardIDToken"] != nil {
		t.Fatalf("an unapproved entry is not an ordinary session entry: %v", oidc)
	}
	requiresTheSession(t, "unapproved", asks.policy())
	if !reflect.DeepEqual(asks.table(), plain.table()) {
		t.Fatalf("an unapproved request changed the bouncer's table:\n%+v\n%+v", asks.table(), plain.table())
	}
	for host, e := range asks.table() {
		if e.KeepClientToken || e.IDTokenSession || e.IDTokenAudience != "" {
			t.Fatalf("%s keeps a header nobody approved: %+v", host, e)
		}
	}

	cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionClientAuthorization)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AwaitingApproval" ||
		!strings.Contains(cond.Message, "web of profile flows") || !strings.Contains(cond.Message, "the header is removed before the app") {
		t.Fatalf("the Component does not say why the app's own calls fail: %+v", cond)
	}
	// A component that asks for nothing carries no such condition.
	if c := apimeta.FindStatusCondition(plain.reconcile().Status.Conditions, ConditionClientAuthorization); c != nil {
		t.Fatalf("a component that asks for nothing: %+v", c)
	}
}

// Approved, the header is left to the app on that host, and the session is
// still required on every route of the component.
func TestAnApprovedEntryKeepsItsHeaderAndStillRequiresTheSession(t *testing.T) {
	h := flowsHarness(t, flowsBundle)
	h.reconcile()
	h.approve(keptApproval("web", nil))
	comp := h.reconcile()

	spec := h.policy()
	requiresTheSession(t, "approved", spec)
	oidc := spec["oidc"].(map[string]interface{})
	// The edge puts no token of its own in the Authorization header, and
	// hands the session to the bouncer as its ID token in a header the
	// filter owns.
	if oidc["forwardAccessToken"] != false {
		t.Fatalf("forwardAccessToken = %v: the edge's token would replace the app's own", oidc["forwardAccessToken"])
	}
	if header := oidc["forwardIDToken"].(map[string]interface{})["header"]; header != bouncer.HeaderIDToken {
		t.Fatalf("forwardIDToken.header = %v, and the bouncer reads %s", header, bouncer.HeaderIDToken)
	}
	// One policy, over both routes: the session is the component's.
	if targets := spec["targetRefs"].([]interface{}); len(targets) != 2 {
		t.Fatalf("targetRefs = %v", targets)
	}

	table := h.table()
	web, hooks := table["flows.acme.k.example"], table["hooks.acme.k.example"]
	if !web.KeepClientToken || web.IDTokenSession || web.IDTokenAudience != "gentian-edge-acme" || web.ForwardToken {
		t.Fatalf("the approved host: %+v", web)
	}
	if web.Relation != "can_use" || web.Object != "app:acme/flows" || web.AuthMode != "oidc" || len(web.SessionCookies) != 2 {
		t.Fatalf("the approved host lost its question or its session cookies: %+v", web)
	}
	// The host that asked for nothing proves its session the same way and
	// still has its header removed.
	if hooks.KeepClientToken || !hooks.IDTokenSession || hooks.IDTokenAudience != "gentian-edge-acme" {
		t.Fatalf("the host that asked for nothing: %+v", hooks)
	}

	cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionClientAuthorization)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Approved" {
		t.Fatalf("condition = %+v", cond)
	}
	// Nothing was published: no DMZ, no proxy.
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: layout.TenantDMZ("acme")}, &corev1.Namespace{}); err == nil {
		t.Fatal("approving an entry behind sign-in made a DMZ namespace")
	}
	if ready := componentReadyCondition(comp); ready != nil && strings.Contains(ready.Message, "published on the perimeter") {
		t.Fatalf("the Component reports a publication: %s", ready.Message)
	}
}

// Withdrawn or expired, the approval is gone and the entry is an ordinary
// one again: the policy, and the annotations a route keeps unless they are
// written over.
func TestAWithdrawnOrExpiredApprovalRemovesTheHeaderAgain(t *testing.T) {
	for what, undo := range map[string]func(h *digestHarness){
		"withdrawn": func(h *digestHarness) { h.approve() },
		"expired": func(h *digestHarness) {
			past := metav1.NewTime(time.Now().Add(-time.Hour))
			on := keptApproval("web", &past)
			on.ReviewAt = metav1.NewTime(time.Now().Add(-2 * time.Hour))
			h.approve(on)
		},
	} {
		h := flowsHarness(t, flowsBundle)
		h.reconcile()
		h.approve(keptApproval("web", nil))
		h.reconcile()
		if !h.table()["flows.acme.k.example"].KeepClientToken {
			t.Fatalf("%s: the approval never took effect", what)
		}
		undo(h)
		comp := h.reconcile()
		if oidc := h.policy()["oidc"].(map[string]interface{}); oidc["forwardAccessToken"] != true || oidc["forwardIDToken"] != nil {
			t.Fatalf("%s: the policy still leaves the header alone: %v", what, oidc)
		}
		for host, e := range h.table() {
			if e.KeepClientToken || e.IDTokenSession || e.IDTokenAudience != "" {
				t.Fatalf("%s: %s still keeps its header: %+v", what, host, e)
			}
		}
		if cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionClientAuthorization); cond == nil || cond.Reason != "AwaitingApproval" {
			t.Fatalf("%s: condition = %+v", what, cond)
		}
	}
}

// An approval of another kind does not keep the header: one given for a
// public address, one that names no kind, one for an entry that did not ask.
func TestOnlyAnApprovalOfThatKindKeepsTheHeader(t *testing.T) {
	review := metav1.NewTime(time.Now().Add(24 * time.Hour))
	for what, on := range map[string]gentianov1alpha1.ExposureEnablement{
		"an approval that names no kind":       {ExposureName: "web", Owner: "u", ReviewAt: review},
		"an approval as a public address":      {ExposureName: "web", Owner: "u", ReviewAt: review, Kind: gentianov1alpha1.ExposureKindPublic},
		"an approval to pass a credential":     {ExposureName: "web", Owner: "u", ReviewAt: review, Kind: gentianov1alpha1.ExposureKindPublicAppCredential},
		"an approval of the entry beside it":   keptApproval("hooks", nil),
		"an approval of an entry there is not": keptApproval("api", nil),
	} {
		h := flowsHarness(t, flowsBundle)
		h.approve(on)
		comp := h.reconcile()
		if oidc := h.policy()["oidc"].(map[string]interface{}); oidc["forwardAccessToken"] != true {
			t.Errorf("%s left the header to the app: %v", what, oidc)
		}
		for host, e := range h.table() {
			if e.KeepClientToken || e.IDTokenSession {
				t.Errorf("%s: %s: %+v", what, host, e)
			}
		}
		// And it publishes nothing, whatever kind it names.
		if err := h.c.Get(context.Background(), types.NamespacedName{Name: layout.TenantDMZ("acme")}, &corev1.Namespace{}); err == nil {
			t.Errorf("%s made a DMZ namespace", what)
		}
		if ready := componentReadyCondition(comp); ready != nil && strings.Contains(ready.Message, "published on the perimeter") {
			t.Errorf("%s: %s", what, ready.Message)
		}
	}
}

// A token the platform puts in the header and an app's own header are never
// the same header: not the session's token (forwardToken), not one exchanged
// for the app (exchangeToken). The schema refuses a profile that declares
// both; a profile that got past it keeps nothing, and what the other entry
// asked for stays as it was.
func TestAnEntryThatIsHandedAPlatformTokenKeepsNoHeader(t *testing.T) {
	for field, exchanged := range map[string]bool{"forwardToken": false, "exchangeToken": true} {
		both := strings.Replace(flowsBundle, "    subDomain: hooks\n", "    subDomain: hooks\n    "+field+": true\n", 1)
		both = strings.Replace(both, "trustTier: certified", "trustTier: platform", 1)
		h := flowsHarness(t, both)
		h.approve(keptApproval("web", nil))
		comp := h.reconcile()
		oidc := h.policy()["oidc"].(map[string]interface{})
		if oidc["forwardAccessToken"] != true || oidc["forwardIDToken"] != nil {
			t.Fatalf("%s: the policy: %v", field, oidc)
		}
		for host, e := range h.table() {
			if e.KeepClientToken || e.IDTokenSession {
				t.Fatalf("%s: %s: %+v", field, host, e)
			}
		}
		if got := h.table()["hooks.acme.k.example"].ExchangeScope != ""; got != exchanged {
			t.Fatalf("%s: the entry that asked for an exchanged token: %+v", field, h.table()["hooks.acme.k.example"])
		}
		if cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionClientAuthorization); cond == nil || cond.Reason != "ForwardsThePlatformToken" {
			t.Fatalf("%s: condition = %+v", field, cond)
		}
	}
}

// Should a table ever be asked for both on one host all the same, the
// exchange goes: where the session is proved by its ID token the
// Authorization header is the client's, and that is nothing to exchange.
func TestNoHostBothKeepsItsHeaderAndExchangesAToken(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "flows", "tenant-acme"
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.example"}, realm: "tenant-acme", clientID: "gentian-edge-acme", cookie: "a", idCookie: "i", sectionName: "x"}
	e := &gentianov1alpha1.ExposureSpec{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC,
		ExchangeToken: true, Backend: gentianov1alpha1.BackendRef{Service: "flows", Port: 8080}}
	for what, authz := range map[string]routeAuthz{
		"kept":          {relation: "can_use", object: "app:acme/flows", keepClientToken: true},
		"beside a kept": {relation: "can_use", object: "app:acme/flows", idTokenSession: true},
	} {
		route := buildExposureRoute(comp, "flows-web", "flows.acme.example", zone, e, authz, "k.example", nil)
		if scope := route.Annotations[bouncerExchangeScopeAnnotation]; scope != "" {
			t.Errorf("%s: the route asks for an exchange of the client's own header: %q", what, scope)
		}
		// And a route that carried the annotation from before is not believed.
		route.Annotations[bouncerExchangeScopeAnnotation] = "gentian-app-flows"
		c := fake.NewClientBuilder().WithScheme(perimeterScheme()).WithObjects(route).Build()
		entries, err := componentRouteTableEntries(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].ExchangeScope != "" {
			t.Errorf("%s: the table: %+v", what, entries)
		}
		table, err := bouncerTable(nil, entries, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bouncer.ParseTable([]byte(table)); err != nil {
			t.Errorf("%s: the bouncer refuses the table the operator wrote: %v", what, err)
		}
	}
	// The bouncer itself refuses a table that says both.
	for _, mode := range []string{"keepClientToken", "idTokenSession"} {
		_, err := bouncer.ParseTable([]byte("routes:\n- {host: h.example, relation: can_use, object: \"app:a/b\", authMode: oidc, " +
			mode + ": true, idTokenAudience: c, exchangeScope: s}\n"))
		if err == nil {
			t.Errorf("a table with %s and an exchange was accepted", mode)
		}
	}
}

// The policies an entry of each kind gets are ones the pinned Envoy Gateway
// release accepts as they are rendered, and each requires the session.
func TestThePoliciesOfEveryEntryRequireTheSession(t *testing.T) {
	zone := edgeZone{
		zoneNames: zoneNames{domain: "acme.example"}, realm: "tenant-acme", clientID: "gentian-edge-acme",
		secretName: "edge-acme-oidc", cookie: "gentian-acme-access", idCookie: "gentian-acme-id",
	}
	for what, authz := range map[string]routeAuthz{
		"an ordinary entry":                         {relation: "can_use", object: "app:acme/flows"},
		"an entry that forwards the platform token": {relation: "can_enter", object: "tenant:acme", forwardToken: true},
		"an entry that keeps the app's own header":  {relation: "can_use", object: "app:acme/flows", keepClientToken: true},
		"a component one of whose hosts keeps it":   {relation: "can_use", object: "app:acme/flows", idTokenSession: true},
	} {
		spec := zoneSecurityPolicySpec("k.example", zone, "flows-web", authz, servicesNamespace, "gentian-os-bouncer")
		requiresTheSession(t, what, spec)
		oidc := spec["oidc"].(map[string]interface{})
		byID := authz.keepClientToken || authz.idTokenSession
		if oidc["forwardAccessToken"] != !byID {
			t.Errorf("%s: forwardAccessToken = %v", what, oidc["forwardAccessToken"])
		}
		if _, has := oidc["forwardIDToken"]; has != byID {
			t.Errorf("%s: forwardIDToken set = %v", what, has)
		}
	}
}

// A route's annotations are kept unless written over, so both are written on
// every route: an approval that is gone leaves nothing behind.
func TestEveryRouteStatesWhetherItsHeaderIsKept(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "flows", "tenant-acme"
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.example"}, realm: "tenant-acme", clientID: "gentian-edge-acme", cookie: "a", idCookie: "i", sectionName: "x"}
	e := &gentianov1alpha1.ExposureSpec{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC,
		Backend: gentianov1alpha1.BackendRef{Service: "flows", Port: 8080}}
	for what, tc := range map[string]struct {
		authz          routeAuthz
		audience, keep string
	}{
		"ordinary":       {routeAuthz{relation: "can_use", object: "app:acme/flows"}, "", ""},
		"kept":           {routeAuthz{relation: "can_use", object: "app:acme/flows", keepClientToken: true}, "gentian-edge-acme", "app"},
		"beside a kept":  {routeAuthz{relation: "can_use", object: "app:acme/flows", idTokenSession: true}, "gentian-edge-acme", ""},
		"forwards token": {routeAuthz{relation: "can_use", object: "app:acme/flows", forwardToken: true}, "", ""},
	} {
		route := buildExposureRoute(comp, "flows-web", "flows.acme.example", zone, e, tc.authz, "k.example", nil)
		audience, said := route.Annotations[bouncerIDTokenAudienceAnnotation]
		keep, saidKeep := route.Annotations[bouncerClientAuthorizationAnnotation]
		if !said || !saidKeep || audience != tc.audience || keep != tc.keep {
			t.Errorf("%s: annotations = %v", what, route.Annotations)
		}
	}
	// The table takes the header for the app's only from a route that also
	// says whose ID token proves the session, and only on a session route.
	scheme := perimeterScheme()
	stray := buildExposureRoute(comp, "stray", "stray.acme.example", zone, e, routeAuthz{relation: "can_use", object: "app:acme/flows"}, "k.example", nil)
	stray.Annotations[bouncerClientAuthorizationAnnotation] = "app"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stray).Build()
	entries, err := componentRouteTableEntries(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].KeepClientToken || entries[0].IDTokenSession {
		t.Fatalf("a route that names no client for the ID token keeps its header: %+v", entries)
	}
}

// The public address that passes its callers' credential to the app.

func davProfile(mode gentianov1alpha1.AuthMode) *gentianov1alpha1.ComponentProfile {
	p := nextcloudBaseProfile()
	p.Spec.Expose = append(p.Spec.Expose, gentianov1alpha1.ExposureSpec{
		Name: "dav", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: mode, SubDomain: "dav",
		Paths: []string{"/remote.php/dav/"}, Backend: gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	})
	return p
}

func davApproval(kind gentianov1alpha1.ExposureKind) []gentianov1alpha1.ExposureEnablement {
	return []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "dav", Owner: "u-tom", Kind: kind, ReviewAt: metav1.NewTime(time.Now().Add(24 * time.Hour)),
	}}
}

// Approved as what it declares, it is published with the one header passed
// on and everything else the proxy does unchanged, at the lower rate. Under
// any other approval, and in any mode the platform does not serve, nothing
// is published at all.
func TestAPublicEntryPassesTheCredentialOnlyWhenApprovedAsThat(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	ctx := context.Background()
	cases := []struct {
		what      string
		mode      gentianov1alpha1.AuthMode
		approval  gentianov1alpha1.ExposureKind
		published bool
		passes    bool
	}{
		{"declared and approved to pass the credential", gentianov1alpha1.AuthModeApp, gentianov1alpha1.ExposureKindPublicAppCredential, true, true},
		{"a plain public address, approved with no kind", gentianov1alpha1.AuthModeNone, "", true, false},
		{"a plain public address, approved as one", gentianov1alpha1.AuthModeNone, gentianov1alpha1.ExposureKindPublic, true, false},
		{"asks to pass the credential, approved before it asked", gentianov1alpha1.AuthModeApp, "", false, false},
		{"asks to pass the credential, approved as a plain address", gentianov1alpha1.AuthModeApp, gentianov1alpha1.ExposureKindPublic, false, false},
		{"a plain address, approved to pass a credential", gentianov1alpha1.AuthModeNone, gentianov1alpha1.ExposureKindPublicAppCredential, false, false},
		{"approved as an entry behind sign-in", gentianov1alpha1.AuthModeApp, gentianov1alpha1.ExposureKindSignInAppAuthorization, false, false},
		{"a password the proxy would have to check", gentianov1alpha1.AuthModeBasic, "", false, false},
		{"a token the edge would have to verify", gentianov1alpha1.AuthModeJWT, gentianov1alpha1.ExposureKindPublicAppCredential, false, false},
		{"a bearer the edge would have to verify", gentianov1alpha1.AuthModeBearer, "", false, false},
		{"a signature the proxy would have to verify", gentianov1alpha1.AuthModeSignature, gentianov1alpha1.ExposureKindPublic, false, false},
	}
	for _, tc := range cases {
		profile := davProfile(tc.mode)
		comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
		comp.Spec.Exposures = davApproval(tc.approval)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), profile, comp).Build()
		r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
		n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant))
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if (n == 1) != tc.published {
			t.Errorf("%s: published %d", tc.what, n)
		}
		// The listener follows the same rule: none for what is not published.
		te := &gentianov1alpha1.TenantExposure{Install: "nextcloud-base-ce", ExposureName: "dav", Kind: tc.approval}
		host := publishedHost(te, map[string]*gentianov1alpha1.ComponentProfile{"nextcloud-base-ce": profile},
			zoneNamesOf(tenant, "k.example", "multi", "kernel"), "k.example")
		if (host != "") != tc.published {
			t.Errorf("%s: listener host = %q", tc.what, host)
		}
		cm := &corev1.ConfigMap{}
		err = c.Get(ctx, types.NamespacedName{Name: perimeterName(comp, "dav"), Namespace: layout.TenantDMZ(tenant.Name)}, cm)
		if !tc.published {
			if err == nil {
				t.Errorf("%s: a proxy was configured", tc.what)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: no proxy configuration: %v", tc.what, err)
		}
		conf := cm.Data["nginx.conf"]
		passed := strings.Contains(conf, "proxy_set_header Authorization $http_authorization;")
		blanked := strings.Contains(conf, `proxy_set_header Authorization "";`)
		if passed != tc.passes || blanked == tc.passes {
			t.Errorf("%s: Authorization passed on = %v, blanked = %v", tc.what, passed, blanked)
		}
	}
}

// An approval that stops covering the entry takes down what was published
// under it: the proxy, its configuration and the route go.
func TestAnApprovalThatNoLongerCoversTheEntryTakesItDown(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	ctx := context.Background()
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	comp.Spec.Exposures = davApproval("")
	plain := davProfile(gentianov1alpha1.AuthModeNone)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), plain, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	if n, err := r.ensurePerimeter(ctx, comp, plain, tenant, r.zoneOf(tenant)); err != nil || n != 1 {
		t.Fatalf("published %d, %v", n, err)
	}
	// The catalogue entry comes to ask for the credential; nobody approved that.
	asks := davProfile(gentianov1alpha1.AuthModeApp)
	if n, err := r.ensurePerimeter(ctx, comp, asks, tenant, r.zoneOf(tenant)); err != nil || n != 0 {
		t.Fatalf("published %d, %v", n, err)
	}
	key := types.NamespacedName{Name: perimeterName(comp, "dav"), Namespace: layout.TenantDMZ(tenant.Name)}
	if err := c.Get(ctx, key, &gatewayv1.HTTPRoute{}); err == nil {
		t.Fatal("the route outlived the approval that covered it")
	}
	if err := c.Get(ctx, key, &corev1.ConfigMap{}); err == nil {
		t.Fatal("the proxy's configuration outlived the approval that covered it")
	}
}

// What the proxy does for such an entry, line by line: the one header goes
// in, and nothing else of what it strips, hides or limits is given up.
func TestTheProxyConfigurationOfAnEntryThatPassesTheCredential(t *testing.T) {
	clearEdgeLimitEnv(t)
	e := &davProfile(gentianov1alpha1.AuthModeApp).Spec.Expose[len(davProfile(gentianov1alpha1.AuthModeApp).Spec.Expose)-1]
	limits := perimeterLimitsFromEnv("")
	passing := perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, true, limits)
	plain := perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, false, limits)

	must := []string{
		"proxy_set_header Authorization $http_authorization;",
		`proxy_set_header Cookie "";`,
		`proxy_set_header X-Forwarded-Access-Token "";`,
		"proxy_hide_header Set-Cookie;",
		"rate=5r/s;",
		"limit_req zone=perimeter_rate burst=50 nodelay;",
		"limit_conn perimeter_concurrent 20;",
		"limit_req_status 429;",
		"limit_conn_status 429;",
		"if ($perimeter_published = 0) { return 404; }",
		"if ($perimeter_ambiguous) { return 400; }",
	}
	for _, h := range perimeterStrippedIdentityHeaders() {
		must = append(must, "proxy_set_header "+h+` "";`)
	}
	for _, h := range perimeterStrippedForwardingHeaders {
		must = append(must, "proxy_set_header "+h+` "";`)
	}
	for _, want := range must {
		if !strings.Contains(passing, want) {
			t.Errorf("an entry that passes the credential lacks %q", want)
		}
	}
	if strings.Contains(passing, `proxy_set_header Authorization "";`) {
		t.Error("the header is both passed and blanked")
	}
	// The same entry not passing it differs in the header and the limits
	// and in nothing else.
	for _, want := range []string{`proxy_set_header Authorization "";`, "rate=20r/s;", "burst=200 nodelay;", "perimeter_concurrent 100;"} {
		if !strings.Contains(plain, want) {
			t.Errorf("an entry that passes nothing lacks %q", want)
		}
	}
	if strings.Contains(plain, "$http_authorization") {
		t.Error("an entry that passes nothing passes the header")
	}
	strip := func(conf string) string {
		var out []string
		for _, line := range strings.Split(conf, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "#") || strings.Contains(l, "Authorization") || strings.HasPrefix(l, "limit_req") || strings.HasPrefix(l, "limit_conn perimeter") {
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	if strip(passing) != strip(plain) {
		t.Fatalf("passing the credential changed more than the header and the limits:\n%s\n---\n%s", strip(passing), strip(plain))
	}
}

// The limits of such an entry are never looser than every other entry's,
// however the cluster's administrator set either.
func TestTheCredentialLimitsAreNeverLooserThanTheOthers(t *testing.T) {
	clearEdgeLimitEnv(t)
	l := perimeterLimitsFromEnv("")
	if l.CredentialRatePerSecond != 5 || l.CredentialBurst != 50 || l.CredentialConcurrent != 20 {
		t.Fatalf("defaults = %+v", l)
	}
	if l.CredentialRatePerSecond >= l.RatePerSecond || l.CredentialBurst >= l.Burst || l.CredentialConcurrent >= l.Concurrent {
		t.Fatalf("the default for an entry that passes a credential is not the stricter one: %+v", l)
	}
	t.Setenv("PERIMETER_CREDENTIAL_RATE_PER_SECOND", "1000")
	t.Setenv("PERIMETER_CREDENTIAL_RATE_BURST", "100000")
	t.Setenv("PERIMETER_CREDENTIAL_CONCURRENT_PER_CLIENT", "5000")
	l = perimeterLimitsFromEnv("")
	if l.CredentialRatePerSecond != l.RatePerSecond || l.CredentialBurst != l.Burst || l.CredentialConcurrent != l.Concurrent {
		t.Fatalf("a setting raised the credential limits past the others: %+v", l)
	}
	t.Setenv("PERIMETER_CREDENTIAL_RATE_PER_SECOND", "2")
	t.Setenv("PERIMETER_RATE_PER_SECOND", "1")
	if l = perimeterLimitsFromEnv(""); l.CredentialRatePerSecond != 1 {
		t.Fatalf("the general rate was lowered below the credential rate, which stayed: %+v", l)
	}
}

// What each entry of a profile asks an approver for, and what an approval
// covers. Both the operator and the director decide by these two.
func TestWhatAnEntryAsksForAndWhatAnApprovalCovers(t *testing.T) {
	entry := func(surface gentianov1alpha1.SurfaceKind, mode gentianov1alpha1.AuthMode, own gentianov1alpha1.ClientAuthorization) *gentianov1alpha1.ExposureSpec {
		return &gentianov1alpha1.ExposureSpec{Surface: surface, AuthMode: mode, ClientAuthorization: own}
	}
	gw, pm := gentianov1alpha1.SurfaceGateway, gentianov1alpha1.SurfacePerimeter
	for _, tc := range []struct {
		e    *gentianov1alpha1.ExposureSpec
		want gentianov1alpha1.ExposureKind
	}{
		{entry(pm, gentianov1alpha1.AuthModeNone, ""), gentianov1alpha1.ExposureKindPublic},
		{entry(pm, gentianov1alpha1.AuthModeApp, ""), gentianov1alpha1.ExposureKindPublicAppCredential},
		{entry(pm, gentianov1alpha1.AuthModeBasic, ""), ""},
		{entry(pm, gentianov1alpha1.AuthModeJWT, ""), ""},
		{entry(pm, gentianov1alpha1.AuthModeBearer, ""), ""},
		{entry(pm, gentianov1alpha1.AuthModeSignature, ""), ""},
		{entry(pm, gentianov1alpha1.AuthModeOIDC, gentianov1alpha1.ClientAuthorizationApp), ""},
		{entry(gw, gentianov1alpha1.AuthModeOIDC, ""), ""},
		{entry(gw, gentianov1alpha1.AuthModeOIDC, gentianov1alpha1.ClientAuthorizationApp), gentianov1alpha1.ExposureKindSignInAppAuthorization},
		{entry(gw, gentianov1alpha1.AuthModeNone, gentianov1alpha1.ClientAuthorizationApp), ""},
		{entry(gw, gentianov1alpha1.AuthModeApp, ""), ""},
	} {
		if got := tc.e.RequestKind(); got != tc.want {
			t.Errorf("%+v asks for %q, want %q", tc.e, got, tc.want)
		}
		for _, approved := range []gentianov1alpha1.ExposureKind{"", gentianov1alpha1.ExposureKindPublic,
			gentianov1alpha1.ExposureKindPublicAppCredential, gentianov1alpha1.ExposureKindSignInAppAuthorization} {
			want := tc.want != "" && approved.Normalized() == tc.want
			if got := tc.e.ApprovedAs(approved); got != want {
				t.Errorf("%+v approved as %q: covered = %v, want %v", tc.e, approved, got, want)
			}
		}
	}
}
