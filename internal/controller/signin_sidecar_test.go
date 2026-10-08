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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// notesHandler is the handler the notes profile's bundle brings.
const notesHandler = "module.exports = { async onLogin(person) { return { redirect: '/home' }; } };\n"

// notesProfile declares a sign-in sidecar and everything it hands its handler.
const notesProfile = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: notes
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/notes
      name: notes
      version: "1.0.0"
  requires:
    services:
      identity:
        sidecar:
          entryPaths: ["/", "/login"]
          database: true
          secrets: [app_secret]
          appPort: 3000
      database:
        engine: postgresql
        databasePerTenant: true
  secrets:
    generated:
      - name: app_secret
        valuePath: appSecret
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      backend:
        service: notes
        port: 3000
`

const notesHandlerCompanion = `apiVersion: v1
kind: ConfigMap
metadata:
  name: notes.sign-in-handler
  labels:
    gentianos.io/profile-name: notes
    gentianos.io/asset: sign-in-handler
data:
  handler.js: "module.exports = { async onLogin(person) { return { redirect: '/home' }; } };\n"
`

func notesBundle() string { return notesProfile + "---\n" + notesHandlerCompanion }

func notesHandlerDigest() string {
	sum := sha256.Sum256([]byte(notesHandler))
	return hex.EncodeToString(sum[:])
}

// notesFromTheClusterCatalogue is the notes profile as a cluster holds it
// after an install from a catalogue of the whole cluster.
func notesFromTheClusterCatalogue(t *testing.T) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	profile := materialised(t, notesBundle())
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	return profile
}

func pinnedNotes(tenant *gentianov1alpha1.Tenant) *gentianov1alpha1.Component {
	comp := componentFor("notes", tenantNamespaceName(tenant))
	comp.Spec.ProfileRef.Digest = profilebundle.Digest([]byte(notesBundle()))
	return comp
}

// A profile that declares a sign-in sidecar gets one where its handler is
// known to be the reviewed one, and nowhere else. Each refusal holds the
// install, with a message that says what is missing.
func TestASignInSidecarRunsOnlyAReviewedHandler(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	tenant := acmeTenantFixture()
	zone := r.zoneOf(tenant)

	sidecar, refusal := r.signInSidecarFor(pinnedNotes(tenant), notesFromTheClusterCatalogue(t), tenant, zone)
	if refusal != "" || sidecar == nil {
		t.Fatalf("a pinned install from a cluster catalogue whose bundle brings the handler was refused: %s", refusal)
	}
	if sidecar.handler != notesHandler || sidecar.handlerDigest != notesHandlerDigest() {
		t.Fatalf("handler %q with digest %s, want the handler of the bundle and its sha256, %s", sidecar.handler, sidecar.handlerDigest, notesHandlerDigest())
	}

	for _, c := range []struct {
		name string
		edit func(*gentianov1alpha1.ComponentProfile, *gentianov1alpha1.Component, *gentianov1alpha1.Tenant)
		want string
	}{
		{"a profile of a tenant's own catalogue",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
			}, "only from a catalogue of the whole cluster"},
		{"a profile nobody says the origin of",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				delete(p.Annotations, profilebundle.OriginAnnotation)
			}, "only from a catalogue of the whole cluster"},
		{"an install that is not pinned",
			func(_ *gentianov1alpha1.ComponentProfile, c *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				c.Spec.ProfileRef.Digest = ""
			}, "not pinned to a digest"},
		{"a bundle that brings no handler",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Annotations[profilebundle.Annotation] = profilebundle.Encode([]byte(notesProfile))
			}, "brings no handler"},
		{"a profile that carries no bundle at all",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				delete(p.Annotations, profilebundle.Annotation)
			}, "brings no handler"},
		{"a tenant that signs in in the kernel realm",
			func(_ *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, tn *gentianov1alpha1.Tenant) {
				tn.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
			}, "kernel realm"},
		{"an entry path that is the sign-in's own",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Identity.Sidecar.EntryPaths = []string{"/sso/acs"}
			}, "the sign-in's own"},
		{"an entry path under the front door's",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Identity.Sidecar.EntryPaths = []string{"/oauth2/callback"}
			}, "the sign-in's own"},
		{"a secret that is not the app's",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Identity.Sidecar.Secrets = []string{"another_apps_key"}
			}, "this app's own secrets only"},
		{"the database of an app that declares none",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Database = nil
			}, "no requires.services.database"},
		{"an entry that is not behind a session",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Expose[0].Surface = gentianov1alpha1.SurfacePerimeter
			}, "needs a gateway entry behind a session"},
		{"an entry that is another component's",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Identity.Sidecar.Exposure = "web"
				p.Spec.Expose[0].Backend.Component = "another"
			}, "not a gateway entry of this component"},
		{"an entry the profile does not have",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				p.Spec.Requires.Services.Identity.Sidecar.Exposure = "admin"
			}, "no such entry"},
		{"two entries and none named",
			func(p *gentianov1alpha1.ComponentProfile, _ *gentianov1alpha1.Component, _ *gentianov1alpha1.Tenant) {
				second := *p.Spec.Expose[0].DeepCopy()
				second.Name, second.SubDomain = "admin", "notes-admin"
				p.Spec.Expose = append(p.Spec.Expose, second)
			}, "must name the one"},
	} {
		tn := acmeTenantFixture()
		profile, comp := notesFromTheClusterCatalogue(t), pinnedNotes(tn)
		c.edit(profile, comp, tn)
		got, refusal := r.signInSidecarFor(comp, profile, tn, r.zoneOf(tn))
		if got != nil || !strings.Contains(refusal, c.want) {
			t.Errorf("%s: sidecar %v, refusal %q; want a refusal saying %q", c.name, got != nil, refusal, c.want)
		}
	}

	// A profile that declares none is not refused for any of this.
	plain := materialised(t, wikiBundle)
	if got, refusal := r.signInSidecarFor(componentFor("wiki", "tenant-acme"), plain, tenant, zone); got != nil || refusal != "" {
		t.Fatalf("a profile that declares no sidecar: %v, %q", got, refusal)
	}
}

// Where the sidecar answers, where the realm is and which realm it is are
// the tenant's, and are handed to the sidecar: a tenant under the cluster's
// domain, the one tenant of a single-tenancy cluster, a tenant on a domain of
// its own, and a tenant whose realm is not called what the tenant is.
func TestTheSidecarsAddressesAreTheTenants(t *testing.T) {
	descriptor := func(realm string) string {
		return "http://gentian-idp-keycloak-keycloakx-http." + layout.Namespace(layout.Authentication) +
			".svc.cluster.local:8080/auth/realms/" + realm + "/protocol/saml/descriptor"
	}
	for _, c := range []struct {
		name, mode string
		tenant     func() *gentianov1alpha1.Tenant
		host       string
		realm      string
	}{
		{"under the cluster's domain", "multi", acmeTenantFixture, "notes.acme.k.example", "acme"},
		{"the one tenant of a single-tenancy cluster", "single", func() *gentianov1alpha1.Tenant {
			tn := &gentianov1alpha1.Tenant{}
			tn.Name = gentianov1alpha1.SingleUserTenantName
			return tn
		}, "notes.k.example", gentianov1alpha1.SingleUserTenantName},
		{"on a domain of its own", "multi", func() *gentianov1alpha1.Tenant {
			tn := acmeTenantFixture()
			tn.Status.Domain = "acme.com"
			return tn
		}, "notes.acme.com", "acme"},
		{"with a realm of another name", "multi", func() *gentianov1alpha1.Tenant {
			tn := acmeTenantFixture()
			tn.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "acme-people"}
			return tn
		}, "notes.acme.k.example", "acme-people"},
	} {
		r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: c.mode}
		tn := c.tenant()
		sidecar, refusal := r.signInSidecarFor(pinnedNotes(tn), notesFromTheClusterCatalogue(t), tn, r.zoneOf(tn))
		if sidecar == nil {
			t.Fatalf("%s: refused: %s", c.name, refusal)
		}
		got := map[string]interface{}{
			"host": sidecar.host, "realm": sidecar.realm,
			"identityProvider": sidecar.identityProvider, "descriptorURL": sidecar.descriptorURL,
			"appURL": sidecar.appURL, "appPort": sidecar.appPort,
			"databaseNamespace": sidecar.databaseNamespace, "databasePort": sidecar.databasePort,
		}
		want := map[string]interface{}{
			"host": c.host, "realm": c.realm,
			"identityProvider": "https://id.k.example/auth/realms/" + c.realm, "descriptorURL": descriptor(c.realm),
			"appURL": "http://notes.tenant-" + tn.Name + ".svc.cluster.local:3000", "appPort": int32(3000),
			"databaseNamespace": layout.System("postgresql"), "databasePort": int32(5432),
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, want)
		}
		// The Composition is told where the app answers and nothing else.
		if !reflect.DeepEqual(sidecar.claim, map[string]interface{}{"host": c.host}) {
			t.Errorf("%s: the claim carries %v, want the host alone", c.name, sidecar.claim)
		}
	}
}

// signInHarness is a pinned install of the notes profile in tenant acme, on a
// cluster whose zone is ready, so that a reconcile reaches the routes.
func signInHarness(t *testing.T) *digestHarness {
	t.Helper()
	bundle := notesBundle()
	h := startDigestHarness(t, notesFromTheClusterCatalogue(t), profilebundle.Digest([]byte(bundle)))
	h.r.KernelRealm, h.r.TenancyMode = "kernel", "multi"
	ctx := context.Background()
	handler := &corev1.ConfigMap{Data: map[string]string{signInHandlerKey: notesHandler}}
	handler.Name, handler.Namespace = "notes."+signInHandlerAsset, layout.Namespace(layout.Provisioning)
	handler.Labels = map[string]string{profilebundle.ProfileLabel: "notes", profilebundle.AssetLabel: signInHandlerAsset}
	zoneSecret := &corev1.Secret{Data: map[string][]byte{"client-secret": []byte("s")}}
	zoneSecret.Name, zoneSecret.Namespace = "edge-acme-oidc", servicesNamespace
	for _, obj := range []client.Object{handler, zoneSecret} {
		if err := h.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *digestHarness) route(name string) *gatewayv1.HTTPRoute {
	h.t.Helper()
	route := &gatewayv1.HTTPRoute{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: h.comp.Namespace}, route); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		h.t.Fatal(err)
	}
	return route
}

func (h *digestHarness) claimSignIn() map[string]interface{} {
	h.t.Helper()
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), claim); err != nil {
		h.t.Fatal(err)
	}
	got, _, _ := unstructured.NestedMap(claim.Object, "spec", "signInSidecar")
	return got
}

func ruleFor(route *gatewayv1.HTTPRoute, path string) *gatewayv1.HTTPRouteRule {
	for i := range route.Spec.Rules {
		for _, m := range route.Spec.Rules[i].Matches {
			if m.Path != nil && m.Path.Value != nil && *m.Path.Value == path {
				return &route.Spec.Rules[i]
			}
		}
	}
	return nil
}

// The two paths of a sidecar are routed apart, and that is the whole design
// of how it meets the front door.
//
// The login path is a rule of the app's own route: behind the zone's session
// and the bouncer's question, like every other path of the app, so nobody
// begins a sign-in who may not use the app. The path the realm posts its
// answer to is a route of its own that no policy names and the bouncer has no
// line for -- the request carries no session, and cannot. Nothing else of the
// app is reachable that way: the route matches one path exactly, by POST, and
// leads to the sidecar alone.
func TestTheSidecarsTwoPathsAreRoutedApart(t *testing.T) {
	h := signInHarness(t)
	ctx := context.Background()
	h.reconcile()

	// The claim carries the sidecar for the Composition.
	if got := h.claimSignIn(); !reflect.DeepEqual(got, map[string]interface{}{"host": "notes.acme.k.example"}) {
		t.Fatalf("the App claim's signInSidecar = %v, want where the app answers", got)
	}

	app := h.route("notes-web")
	if app == nil {
		t.Fatal("the app's own route is not there")
	}
	login := ruleFor(app, signInLoginPath)
	if login == nil || *login.Matches[0].Path.Type != gatewayv1.PathMatchExact ||
		string(login.BackendRefs[0].Name) != "notes-sign-in" || int32(*login.BackendRefs[0].Port) != signInSidecarPort {
		t.Fatalf("the login path is not a rule of the app's route to the sidecar: %+v", login)
	}
	for _, entry := range []string{"/", "/login"} {
		var redirect *gatewayv1.HTTPRouteRule
		for i := range app.Spec.Rules {
			m := app.Spec.Rules[i].Matches[0]
			if m.Path != nil && *m.Path.Value == entry && *m.Path.Type == gatewayv1.PathMatchExact {
				redirect = &app.Spec.Rules[i]
			}
		}
		if redirect == nil || len(redirect.BackendRefs) != 0 || redirect.Matches[0].Method == nil || *redirect.Matches[0].Method != gatewayv1.HTTPMethodGet ||
			redirect.Filters[0].RequestRedirect == nil || *redirect.Filters[0].RequestRedirect.Path.ReplaceFullPath != signInLoginPath {
			t.Fatalf("entry path %s does not lead to the login path by a GET-only redirect: %+v", entry, redirect)
		}
	}
	// The app's own rule for everything else is still there, and still the app's.
	var whole *gatewayv1.HTTPRouteRule
	for i := range app.Spec.Rules {
		m := app.Spec.Rules[i].Matches[0]
		if m.Path != nil && *m.Path.Value == "/" && *m.Path.Type == gatewayv1.PathMatchPathPrefix {
			whole = &app.Spec.Rules[i]
		}
	}
	if whole == nil || string(whole.BackendRefs[0].Name) != "notes" {
		t.Fatalf("the app's own rule is gone or leads elsewhere: %+v", whole)
	}
	if ruleFor(app, signInACSPath) != nil {
		t.Fatal("the answer's path is a rule of the route behind the session, where no answer could arrive")
	}

	acs := h.route(signInACSRouteName("notes", "web"))
	if acs == nil {
		t.Fatal("the route for the realm's answer is not there")
	}
	if len(acs.Spec.Rules) != 1 || len(acs.Spec.Rules[0].Matches) != 1 {
		t.Fatalf("the answer's route has %d rules; it has one, with one match", len(acs.Spec.Rules))
	}
	rule, match := acs.Spec.Rules[0], acs.Spec.Rules[0].Matches[0]
	if *match.Path.Type != gatewayv1.PathMatchExact || *match.Path.Value != signInACSPath || match.Method == nil || *match.Method != gatewayv1.HTTPMethodPost {
		t.Fatalf("the answer's route matches %+v; it matches %s exactly, by POST", match, signInACSPath)
	}
	if len(rule.BackendRefs) != 1 || string(rule.BackendRefs[0].Name) != "notes-sign-in" {
		t.Fatalf("the answer's route leads to %+v, not to the sidecar alone", rule.BackendRefs)
	}
	if string(acs.Spec.Hostnames[0]) != "notes.acme.k.example" || string(app.Spec.Hostnames[0]) != "notes.acme.k.example" {
		t.Fatalf("the two routes are on %v and %v, not on the app's one host", acs.Spec.Hostnames, app.Spec.Hostnames)
	}
	if *acs.Spec.ParentRefs[0].SectionName != *app.Spec.ParentRefs[0].SectionName || string(acs.Spec.ParentRefs[0].Name) != AuthenticatedGatewayName {
		t.Fatal("the answer's route is not on the app's listener")
	}
	// Nothing on this route overwrites the identity headers, so they are removed.
	var removed []string
	for _, f := range rule.Filters {
		if f.RequestHeaderModifier != nil {
			removed = append(removed, f.RequestHeaderModifier.Remove...)
		}
	}
	for _, name := range []string{"x-gentian-subject", "x-gentian-realm", "x-gentian-session", "x-gentian-email", "x-gentian-name"} {
		found := false
		for _, r := range removed {
			found = found || r == name
		}
		if !found {
			t.Fatalf("the answer's route does not remove %s, which a client could then send", name)
		}
	}

	// It is the component's, so removing the app removes it.
	if owner := metav1.GetControllerOf(acs); owner == nil || owner.Kind != "Component" || owner.Name != "notes" {
		t.Fatalf("the answer's route is owned by %+v, so removing the app would leave a path with no session behind", owner)
	}

	// No session and no question on it: no bouncer label, no line in the
	// table, and the component's one policy names the app's route only.
	if acs.Labels[bouncerRouteLabel] != "" || len(acs.Annotations) != 0 {
		t.Fatalf("the answer's route carries a question: labels %v, annotations %v", acs.Labels, acs.Annotations)
	}
	table, err := componentRouteTableEntries(ctx, h.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 || table[0].Host != "notes.acme.k.example" || table[0].AuthMode != "oidc" {
		t.Fatalf("the bouncer's table = %+v; the host has one line, behind a session", table)
	}
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(schema.GroupVersionKind{Group: securityPolicyGVK.Group, Version: securityPolicyGVK.Version, Kind: "SecurityPolicyList"})
	if err := h.c.List(ctx, policies, client.InNamespace(h.comp.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 1 {
		t.Fatalf("%d policies, want the component's one", len(policies.Items))
	}
	targets, _, _ := unstructured.NestedSlice(policies.Items[0].Object, "spec", "targetRefs")
	if len(targets) != 1 || targets[0].(map[string]interface{})["name"] != "notes-web" {
		t.Fatalf("the policy names %v; it names the app's route and never the answer's", targets)
	}
}

// A profile that stops declaring the sidecar loses it: the claim no longer
// carries it, which is what makes the Composition take the sidecar and its
// client at the realm away, and the route that took no session is deleted.
func TestASidecarNoLongerDeclaredIsTakenAway(t *testing.T) {
	h := signInHarness(t)
	ctx := context.Background()
	h.reconcile()
	if h.route(signInACSRouteName("notes", "web")) == nil || h.claimSignIn() == nil || len(h.sidecarObjects()) != 6 {
		t.Fatalf("the sidecar was not there to begin with: %v", h.sidecarObjects())
	}

	// The next build of the profile declares none, and the install moves to it.
	next := strings.Replace(notesProfile, `      identity:
        sidecar:
          entryPaths: ["/", "/login"]
          database: true
          secrets: [app_secret]
          appPort: 3000
`, "", 1)
	if next == notesProfile {
		t.Fatal("the fixture's declaration was not found")
	}
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes"}, profile); err != nil {
		t.Fatal(err)
	}
	fresh := materialised(t, next)
	fresh.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	profile.Spec, profile.Annotations = fresh.Spec, fresh.Annotations
	if err := h.c.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(h.comp), comp); err != nil {
		t.Fatal(err)
	}
	comp.Spec.ProfileRef.Digest = profilebundle.Digest([]byte(next))
	if err := h.c.Update(ctx, comp); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	if got := h.claimSignIn(); got != nil {
		t.Fatalf("the claim still carries the sidecar: %v", got)
	}
	if h.route(signInACSRouteName("notes", "web")) != nil {
		t.Fatal("the route that takes no session outlived the sidecar")
	}
	if left := h.sidecarObjects(); len(left) != 0 {
		t.Fatalf("the sidecar outlived its declaration: %v", left)
	}
	app := h.route("notes-web")
	if app == nil || ruleFor(app, signInLoginPath) != nil || len(app.Spec.Rules) != 1 {
		t.Fatalf("the app's route still carries the sidecar's rules: %+v", app)
	}
}

// A profile that may not have a sidecar is held before anything is written:
// no claim, no route, no way in that leads nowhere.
func TestAProfileThatMayNotHaveASidecarIsNotInstalled(t *testing.T) {
	bundle := notesBundle()
	profile := materialised(t, bundle)
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
	h := startDigestHarness(t, profile, profilebundle.Digest([]byte(bundle)))
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" {
		t.Fatalf("condition = %+v, want the install held", ready)
	}
	// A tenant's own catalogue brings no companions at all, which the bundle
	// check says first; a sidecar's own refusal is the second lock.
	if ready.Reason != profilebundle.ReasonRefused && ready.Reason != reasonSignInSidecarRefused {
		t.Fatalf("reason = %s", ready.Reason)
	}
	if h.route("notes-web") != nil || h.route(signInACSRouteName("notes", "web")) != nil {
		t.Fatal("a route was written for an install that is held")
	}

	// The same profile without its handler passes the bundle check and is
	// held by the sidecar's own rule.
	alone := materialised(t, notesProfile)
	alone.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
	h = startDigestHarness(t, alone, profilebundle.Digest([]byte(notesProfile)))
	ready = componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != reasonSignInSidecarRefused ||
		!strings.Contains(ready.Message, "only from a catalogue of the whole cluster") {
		t.Fatalf("condition = %+v, want %s", ready, reasonSignInSidecarRefused)
	}
	if h.route("notes-web") != nil {
		t.Fatal("a route was written for an install that is held")
	}
}

// The reconciler and the Composition name the same client: the sidecar is
// told its own name and address by one, and registered at the realm under
// them by the other. A name that differed would be a sign-in the realm
// answers with "unknown client".
func TestTheSignInSidecarHasOneName(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "compositions", "app-default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	composition := string(raw)
	for _, want := range []string{
		`$signInEntity := printf "https://%s/sso" $signIn.host`,
		`$signInACS := printf "https://%s` + signInACSPath + `" $signIn.host`,
		"clientId: {{ $signInEntity | quote }}",
		"assertionConsumerPostUrl: {{ $signInACS | quote }}",
	} {
		if !strings.Contains(composition, want) {
			t.Errorf("app-default.yaml does not say %q, which the reconciler relies on", want)
		}
	}
	tenant := acmeTenantFixture()
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	sidecar, refusal := r.signInSidecarFor(pinnedNotes(tenant), notesFromTheClusterCatalogue(t), tenant, r.zoneOf(tenant))
	if sidecar == nil {
		t.Fatal(refusal)
	}
	env := map[string]string{}
	for _, e := range signInDeployment(pinnedNotes(tenant), sidecar, nil, false, pullSecrets{}).Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SSO_ENTITY_ID"] != "https://notes.acme.k.example/sso" || env["SSO_ACS_URL"] != "https://notes.acme.k.example/sso/acs" {
		t.Fatalf("the sidecar is told it is %s at %s", env["SSO_ENTITY_ID"], env["SSO_ACS_URL"])
	}
}

// The sidecar's image is one build: a tag that names a commit or a version,
// with the digest of what it held. Never a name a registry may point
// elsewhere -- this is the program that decides whether a sign-in is genuine.
func TestTheSignInSidecarImageIsOneBuild(t *testing.T) {
	image := kernel.DefaultSignInSidecarImage
	name, digest, pinned := strings.Cut(image, "@sha256:")
	if !pinned || len(digest) != 64 {
		t.Fatalf("%s names no digest", image)
	}
	_, tag, tagged := strings.Cut(name[strings.LastIndex(name, "/"):], ":")
	if !tagged || tag == "latest" || tag == "develop" || tag == "main" {
		t.Fatalf("%s is tagged %q, which is not the name of one build", image, tag)
	}
	if kernel.SignInSidecarImage() != image {
		t.Fatalf("the sidecar runs %s", kernel.SignInSidecarImage())
	}
}

// The two profiles of the catalogue that declare a sign-in sidecar, as
// gentian-apps publishes them (internal/profilebundle/testdata/bundles), are
// served: each brings its handler, names its own secrets and stands on its
// own entry. A catalogue and a platform that disagreed about the declaration
// would install the app with no way in.
func TestTheCataloguesSidecarProfilesAreServed(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	tenant := acmeTenantFixture()
	for name, want := range map[string]struct{ host, appURL string }{
		"activepieces-me": {"auto.acme.k.example", "http://activepieces-me.tenant-acme.svc.cluster.local:8080"},
		"docmost-ce":      {"docs.acme.k.example", "http://docmost-ce.tenant-acme.svc.cluster.local:3000"},
	} {
		raw, err := os.ReadFile(filepath.Join("..", "profilebundle", "testdata", "bundles", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		profile := materialised(t, string(raw))
		profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("gentian")
		comp := componentFor(name, tenantNamespaceName(tenant))
		comp.Spec.ProfileRef.Digest = profilebundle.Digest(raw)

		sidecar, refusal := r.signInSidecarFor(comp, profile, tenant, r.zoneOf(tenant))
		if sidecar == nil {
			t.Fatalf("%s: %s", name, refusal)
		}
		if sidecar.host != want.host || sidecar.appURL != want.appURL {
			t.Errorf("%s: host %v, appURL %v; want %s and %s", name, sidecar.host, sidecar.appURL, want.host, want.appURL)
		}
		if !sidecar.database || sidecar.databaseNamespace != layout.System("postgresql") || len(sidecar.secrets) != 1 {
			t.Errorf("%s: its handler is not given its database and its one secret: %+v", name, sidecar)
		}
		// The app's own sign-in is refused at the front door, and the page
		// the handler ends on is not one that leads back to the sign-in.
		if len(sidecar.exposure.DenyPaths) == 0 {
			t.Errorf("%s: the app's own sign-in calls are not refused", name)
		}
		if len(sidecar.entryPaths) == 0 {
			t.Errorf("%s: no page of the app leads to the sign-in", name)
		}
		// The same from a tenant's own catalogue is not served.
		profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
		if got, refusal := r.signInSidecarFor(comp, profile, tenant, r.zoneOf(tenant)); got != nil || refusal == "" {
			t.Errorf("%s from a tenant's own catalogue was served", name)
		}
	}
}

// sidecarObjects names what the notes component has of a sidecar.
func (h *digestHarness) sidecarObjects() []string {
	h.t.Helper()
	ctx := context.Background()
	in, selector := client.InNamespace(h.comp.Namespace), client.MatchingLabels{signInSidecarLabel: h.comp.Name}
	var out []string
	secretsList := &unstructured.UnstructuredList{}
	secretsList.SetGroupVersionKind(schema.GroupVersionKind{Group: externalSecretGVK.Group, Version: externalSecretGVK.Version, Kind: externalSecretGVK.Kind + "List"})
	for kind, list := range map[string]client.ObjectList{
		"ExternalSecret": secretsList, "Deployment": &appsv1.DeploymentList{}, "Service": &corev1.ServiceList{},
		"ConfigMap": &corev1.ConfigMapList{}, "NetworkPolicy": &networkingv1.NetworkPolicyList{},
	} {
		if err := h.c.List(ctx, list, in, selector); err != nil {
			h.t.Fatal(err)
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, item := range items {
			obj := item.(client.Object)
			if owner := metav1.GetControllerOf(obj); owner == nil || owner.Name != h.comp.Name || owner.Kind != "Component" {
				h.t.Fatalf("%s %s is not owned by the component, so removing the app would leave it: %+v", kind, obj.GetName(), owner)
			}
			out = append(out, kind+" "+obj.GetName())
		}
	}
	sort.Strings(out)
	return out
}

// The sidecar the operator writes beside an app: what runs, what it is told,
// what it is handed and what it may reach.
func TestTheSidecarIsWrittenBesideTheApp(t *testing.T) {
	h := signInHarness(t)
	ctx := context.Background()
	h.reconcile()
	ns := h.comp.Namespace

	want := []string{
		"ConfigMap notes-sign-in-handler", "Deployment notes-sign-in", "ExternalSecret notes-sign-in",
		"NetworkPolicy notes-sign-in", "NetworkPolicy notes-sign-in-to-app", "Service notes-sign-in",
	}
	if got := h.sidecarObjects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the sidecar is %v, want %v", got, want)
	}

	// The handler is the bundle's, byte for byte.
	handler := &corev1.ConfigMap{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in-handler", Namespace: ns}, handler); err != nil {
		t.Fatal(err)
	}
	if handler.Data[signInHandlerKey] != notesHandler || len(handler.Data) != 1 {
		t.Fatalf("the handler the sidecar is given is not the bundle's: %q", handler.Data)
	}

	deploy := &appsv1.Deployment{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: ns}, deploy); err != nil {
		t.Fatal(err)
	}
	pod, c := deploy.Spec.Template.Spec, deploy.Spec.Template.Spec.Containers[0]
	if *deploy.Spec.Replicas != 1 || deploy.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("%d replicas, %s: a sign-in under way is in one process, so there is one and never two", *deploy.Spec.Replicas, deploy.Spec.Strategy.Type)
	}
	if c.Image != kernel.SignInSidecarImage() {
		t.Fatalf("the sidecar runs %s", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Fatalf("%s is read from somewhere: every setting is a value the platform states", e.Name)
		}
		env[e.Name] = e.Value
	}
	wantEnv := map[string]string{
		"SSO_ENTITY_ID":          "https://notes.acme.k.example/sso",
		"SSO_ACS_URL":            "https://notes.acme.k.example/sso/acs",
		"SSO_LOGIN_PATH":         "/sso/login",
		"SSO_IDP_ENTITY_ID":      "https://id.k.example/auth/realms/acme",
		"SSO_IDP_SSO_URL":        "https://id.k.example/auth/realms/acme/protocol/saml",
		"SSO_IDP_DESCRIPTOR_URL": "http://gentian-idp-keycloak-keycloakx-http." + layout.Namespace(layout.Authentication) + ".svc.cluster.local:8080/auth/realms/acme/protocol/saml/descriptor",
		"SSO_REALM":              "acme",
		"SSO_HANDLER_SHA256":     notesHandlerDigest(),
		"APP_URL":                "http://notes.tenant-acme.svc.cluster.local:3000",
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("the sidecar is told\n%v\nwant\n%v", env, wantEnv)
	}
	if deploy.Spec.Template.Annotations["gentianos.io/sign-in-handler"] != notesHandlerDigest() {
		t.Fatal("a new handler would not start a new process")
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "notes-sign-in" {
		t.Fatalf("what the handler is handed comes from %+v, want the one Secret made for it", c.EnvFrom)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("the sidecar's pod is not locked down: no token, not root, read-only, nothing kept, default seccomp")
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap == nil || pod.Volumes[0].ConfigMap.Name != "notes-sign-in-handler" || !c.VolumeMounts[0].ReadOnly {
		t.Fatalf("the sidecar mounts %+v, want its handler and nothing else, read-only", pod.Volumes)
	}
	// Not one of the app's pods: none of the app's own network paths, and
	// nothing that selects the app selects it.
	if _, isApp := deploy.Spec.Template.Labels["gentianos.io/app"]; isApp || deploy.Spec.Template.Labels[signInSidecarLabel] != "notes" {
		t.Fatalf("the sidecar's pod is labelled %v", deploy.Spec.Template.Labels)
	}

	// What it is handed: this app's own database and the one secret the
	// profile named, from this app's own vault paths.
	handed := &unstructured.Unstructured{}
	handed.SetGroupVersionKind(externalSecretGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: ns}, handed); err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedSlice(handed.Object, "spec", "data")
	got := map[string]string{}
	for _, entry := range data {
		e := entry.(map[string]interface{})
		ref := e["remoteRef"].(map[string]interface{})
		got[e["secretKey"].(string)] = ref["key"].(string) + "#" + ref["property"].(string)
	}
	wantData := map[string]string{
		"DB_HOST":           "gentian-os/tenants/acme/apps/notes/database#host",
		"DB_PORT":           "gentian-os/tenants/acme/apps/notes/database#port",
		"DB_NAME":           "gentian-os/tenants/acme/apps/notes/database#name",
		"DB_USER":           "gentian-os/tenants/acme/apps/notes/database#user",
		"DB_PASSWORD":       "gentian-os/tenants/acme/apps/notes/database#password",
		"SECRET_APP_SECRET": "gentian-os/tenants/acme/apps/notes/internal/app_secret#value",
	}
	if !reflect.DeepEqual(got, wantData) {
		t.Fatalf("the handler is handed\n%v\nwant\n%v", got, wantData)
	}
	if target, _, _ := unstructured.NestedString(handed.Object, "spec", "target", "name"); target != "notes-sign-in" {
		t.Fatalf("the values land in %q", target)
	}

	// What it may reach: three places, one port each.
	policy := &networkingv1.NetworkPolicy{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: ns}, policy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, map[string]string{signInSidecarLabel: "notes"}) ||
		len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("the sidecar's policy selects %v for %v", policy.Spec.PodSelector.MatchLabels, policy.Spec.PolicyTypes)
	}
	var reach []string
	for _, rule := range policy.Spec.Egress {
		if len(rule.To) != 1 || len(rule.Ports) != 1 || rule.Ports[0].Port == nil {
			t.Fatalf("a path that is not one place on one port: %+v", rule)
		}
		to := rule.To[0]
		where := ""
		switch {
		case to.NamespaceSelector != nil && to.PodSelector == nil:
			where = "namespace " + to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
		case to.PodSelector != nil && to.NamespaceSelector == nil:
			where = "pods gentianos.io/app=" + to.PodSelector.MatchLabels["gentianos.io/app"]
		default:
			t.Fatalf("a path to %+v", to)
		}
		reach = append(reach, where+":"+rule.Ports[0].Port.String())
	}
	wantReach := []string{
		"namespace " + layout.Namespace(layout.Authentication) + ":8080",
		"namespace " + layout.System("postgresql") + ":5432",
		"pods gentianos.io/app=notes:3000",
	}
	if !reflect.DeepEqual(reach, wantReach) {
		t.Fatalf("the sidecar may reach %v, want %v", reach, wantReach)
	}
	toApp := &networkingv1.NetworkPolicy{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in-to-app", Namespace: ns}, toApp); err != nil {
		t.Fatal(err)
	}
	in := toApp.Spec.Ingress
	if toApp.Spec.PodSelector.MatchLabels["gentianos.io/app"] != "notes" || len(in) != 1 || len(in[0].From) != 1 ||
		in[0].From[0].PodSelector.MatchLabels[signInSidecarLabel] != "notes" || in[0].Ports[0].Port.IntValue() != 3000 {
		t.Fatalf("the app admits %+v", toApp.Spec)
	}

	// A second pass changes nothing.
	before := deploy.ResourceVersion
	h.reconcile()
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: ns}, deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.ResourceVersion != before {
		t.Fatal("the sidecar is rewritten on every pass")
	}
}

// A handler that is given nothing gets nothing: no Secret, no path to a
// database, none to the app.
func TestAHandlerIsHandedOnlyWhatItsProfileDeclared(t *testing.T) {
	bare := strings.Replace(notesProfile, `          entryPaths: ["/", "/login"]
          database: true
          secrets: [app_secret]
          appPort: 3000
`, `          entryPaths: ["/"]
`, 1)
	if bare == notesProfile {
		t.Fatal("the fixture's declaration was not found")
	}
	bundle := bare + "---\n" + notesHandlerCompanion
	profile := materialised(t, bundle)
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	h := startDigestHarness(t, profile, profilebundle.Digest([]byte(bundle)))
	h.r.KernelRealm, h.r.TenancyMode = "kernel", "multi"
	ctx := context.Background()
	handler := &corev1.ConfigMap{Data: map[string]string{signInHandlerKey: notesHandler}}
	handler.Name, handler.Namespace = "notes."+signInHandlerAsset, layout.Namespace(layout.Provisioning)
	handler.Labels = map[string]string{profilebundle.ProfileLabel: "notes", profilebundle.AssetLabel: signInHandlerAsset}
	zoneSecret := &corev1.Secret{Data: map[string][]byte{"client-secret": []byte("s")}}
	zoneSecret.Name, zoneSecret.Namespace = "edge-acme-oidc", servicesNamespace
	for _, obj := range []client.Object{handler, zoneSecret} {
		if err := h.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	h.reconcile()

	want := []string{"ConfigMap notes-sign-in-handler", "Deployment notes-sign-in", "NetworkPolicy notes-sign-in", "Service notes-sign-in"}
	if got := h.sidecarObjects(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the sidecar is %v, want %v", got, want)
	}
	deploy := &appsv1.Deployment{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: h.comp.Namespace}, deploy); err != nil {
		t.Fatal(err)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if len(c.EnvFrom) != 0 {
		t.Fatalf("the handler is handed %+v", c.EnvFrom)
	}
	for _, e := range c.Env {
		if e.Name == "APP_URL" || strings.HasPrefix(e.Name, "DB_") || strings.HasPrefix(e.Name, "SECRET_") {
			t.Fatalf("the handler is told %s", e.Name)
		}
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := h.c.Get(ctx, types.NamespacedName{Name: "notes-sign-in", Namespace: h.comp.Namespace}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Egress) != 1 || policy.Spec.Egress[0].Ports[0].Port.IntValue() != 8080 {
		t.Fatalf("the sidecar may reach %+v, want the realm's certificate alone", policy.Spec.Egress)
	}
}

// The two routes of a sidecar, created against the Gateway API's own
// definitions with their rules enforced: a path matched exactly with a
// method, a redirect that replaces the whole path, a rule that removes
// request headers. The definitions are the ones of the module this operator
// is built with; a route they refuse is a route no Gateway ever serves, and
// for the one that carries the realm's answer that is a sign-in that cannot
// complete.
func TestTheSidecarsRoutesAreAdmittedByTheGatewayAPI(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		t.Fatalf("where the Gateway API module is: %v", err)
	}
	definition := filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard", "gateway.networking.k8s.io_httproutes.yaml")
	binDir := "/tmp/envtest-bins/k8s/1.32.0-linux-amd64"
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		binDir = v
	}
	env := &envtest.Environment{
		CRDInstallOptions:     envtest.CRDInstallOptions{Paths: []string{definition}, ErrorIfPathMissing: true},
		BinaryAssetsDirectory: binDir,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("the Gateway API's definitions do not install: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	tenant := acmeTenantFixture()
	profile, comp := notesFromTheClusterCatalogue(t), pinnedNotes(tenant)
	zone := r.zoneOf(tenant)
	sidecar, refusal := r.signInSidecarFor(comp, profile, tenant, zone)
	if sidecar == nil {
		t.Fatal(refusal)
	}
	ns := &corev1.Namespace{}
	ns.Name = comp.Namespace
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	host := exposureHost(zone, comp, sidecar.exposure)
	framers := componentFramers(zone, comp, profile, host)
	app := buildExposureRoute(comp, "notes-web", host, zone, sidecar.exposure,
		exposureAuthz(tenant, comp, profile, false), r.KernelDomain, framers)
	app.Spec.Rules = append(signInRouteRules(comp.Namespace, sidecar, frameAncestorsFilters(framers)...), app.Spec.Rules...)
	for what, route := range map[string]*gatewayv1.HTTPRoute{
		"the app's route with the sidecar's rules": app,
		"the route for the realm's answer":         buildSignInACSRoute(comp, zone, sidecar, r.KernelDomain),
	} {
		if err := c.Create(ctx, route, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Errorf("%s is refused: %v", what, err)
		}
	}
}
