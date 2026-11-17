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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// deviceWay is a way from a program on a person's device to the app.
func deviceWay(name string, protocol gentianov1alpha1.BridgeProtocolName, paths ...string) gentianov1alpha1.BridgeWay {
	return gentianov1alpha1.BridgeWay{
		Name: name, Caller: gentianov1alpha1.BridgeCallerDevice, Callee: gentianov1alpha1.BridgeCalleeApp,
		Protocol: protocol, SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson, Paths: paths,
	}
}

func bridgeProfile(name, app string, ways ...gentianov1alpha1.BridgeWay) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = name
	p.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge}
	p.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{App: &gentianov1alpha1.BridgedApp{Profile: app}, Ways: ways}
	p.Spec.Package.API = &gentianov1alpha1.APIIntegration{Backend: &gentianov1alpha1.APIBackend{Service: app, Port: 8080}}
	return p
}

func nextcloudBridge() *gentianov1alpha1.ComponentProfile {
	return bridgeProfile("nextcloud-dav", "nextcloud-base-ce",
		deviceWay("files", gentianov1alpha1.BridgeProtocolWebDAV, "/remote.php/dav/files/"),
		deviceWay("calendars", gentianov1alpha1.BridgeProtocolCalDAV, "/remote.php/dav/calendars/", "/remote.php/dav/principals/"),
		deviceWay("contacts", gentianov1alpha1.BridgeProtocolCardDAV, "/remote.php/dav/addressbooks/", "/remote.php/dav/principals"),
	)
}

var bridgeZone = edgeZone{
	zoneNames: zoneNames{domain: "acme.k.example"}, realm: "acme", clientID: "gentian-edge-acme",
	secretName: "edge-acme-oidc", cookie: "gentian-acme-access", idCookie: "gentian-acme-id",
	sectionName: tenantGatewayListenerName("acme"),
}

// bridgeRoute is the route the reconciler writes for a served bridge.
func bridgeRoute(t *testing.T, profile *gentianov1alpha1.ComponentProfile, created time.Time) *gatewayv1.HTTPRoute {
	t.Helper()
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "acme"
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = profile.Name, "tenant-acme"
	e, door := bridgeEntryOf(profile), bridgeDoorOf(tenant, profile)
	if e == nil || door == nil {
		t.Fatalf("%s is not read as a bridge", profile.Name)
	}
	route := buildExposureRoute(comp, "acme", tokenEntryRouteName(comp.Name, e.Name), exposureHost(bridgeZone, comp, e), bridgeZone, e, door.authz, "k.example", nil)
	door.annotate(route)
	route.CreationTimestamp = metav1.NewTime(created)
	return route
}

// A bridge is read as one entry at the tenant's door: it takes a token, it
// routes the bridge's paths and no other to the Service the bridge names, and
// what is asked for is approved as a token entry.
func TestABridgeIsServedAsATokenEntryAtTheDoor(t *testing.T) {
	profile := nextcloudBridge()
	e := bridgeEntryOf(profile)
	if e == nil {
		t.Fatal("no entry")
	}
	if e.RequestKind() != gentianov1alpha1.ExposureKindTokenEntry || !e.ApprovedAs(gentianov1alpha1.ExposureKindTokenEntry) || e.ApprovedAs(gentianov1alpha1.ExposureKindPublic) {
		t.Fatalf("a bridge asks for %q", e.RequestKind())
	}
	want := []string{"/remote.php/dav/addressbooks", "/remote.php/dav/calendars", "/remote.php/dav/files", "/remote.php/dav/principals"}
	if !reflect.DeepEqual(e.Paths, want) {
		t.Fatalf("paths %v, want %v", e.Paths, want)
	}

	route := bridgeRoute(t, profile, time.Unix(100, 0))
	if got := route.Spec.Hostnames; len(got) != 1 || got[0] != "sync.acme.k.example" {
		t.Fatalf("hosts %v", got)
	}
	if len(route.Spec.Rules) != len(want) {
		t.Fatalf("%d rules for %d paths", len(route.Spec.Rules), len(want))
	}
	for i, rule := range route.Spec.Rules {
		m := rule.Matches[0].Path
		if *m.Type != gatewayv1.PathMatchPathPrefix || *m.Value != want[i] {
			t.Errorf("rule %d matches %s %s", i, *m.Type, *m.Value)
		}
		if b := rule.BackendRefs[0]; string(b.Name) != "nextcloud-base-ce" || int32(*b.Port) != 8080 {
			t.Errorf("rule %d goes to %s:%d", i, b.Name, *b.Port)
		}
	}
	for key, value := range map[string]string{
		bouncerAuthModeAnnotation:         "bearer",
		bouncerRelationAnnotation:         "can_use",
		bouncerObjectAnnotation:           "app:acme/nextcloud-base-ce",
		bouncerExchangeScopeAnnotation:    "bridge-nextcloud-base-ce",
		bouncerDoorPathsAnnotation:        strings.Join(want, ","),
		bouncerDoorObjectAnnotation:       "tenant:acme",
		bouncerTokenAudienceAnnotation:    "https://sync.acme.k.example",
		bouncerTokenRealmAnnotation:       "acme",
		bouncerResourceMetadataAnnotation: "https://sync.acme.k.example/.well-known/oauth-protected-resource",
		bouncerForwardAnnotation:          "false",
	} {
		if got := route.Annotations[key]; got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

// What is not an external bridge routed to a Service has no entry at the
// door: an app, an adapter, a bridge that would run a workload of its own.
func TestOnlyABridgeRoutedToAServiceHasAnEntryAtTheDoor(t *testing.T) {
	app := &gentianov1alpha1.ComponentProfile{}
	app.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}
	app.Spec.Package.API = &gentianov1alpha1.APIIntegration{BaseURL: "https://service.example"}

	workload := nextcloudBridge()
	workload.Spec.Package.API = nil
	workload.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Name: "carddav"}

	mislabeled := nextcloudBridge()
	mislabeled.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}

	for name, p := range map[string]*gentianov1alpha1.ComponentProfile{
		"nil": nil, "an app packaged as an api": app, "a bridge that runs a workload": workload,
		"a bridge block on another class": mislabeled,
	} {
		if e := bridgeEntryOf(p); e != nil {
			t.Errorf("%s: %+v", name, e)
		}
		if d := bridgeDoorOf(&gentianov1alpha1.Tenant{}, p); d != nil {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

// Two bridges of a tenant answer on one host, each under its own paths with
// its own app's question, and the bouncer reads the table as one door.
func TestTheRouteTableMakesOneDoorOfATenantsBridges(t *testing.T) {
	files := bridgeRoute(t, nextcloudBridge(), time.Unix(100, 0))
	contacts := bridgeRoute(t, bridgeProfile("odoo-contacts", "odoo-base-ce",
		deviceWay("way", gentianov1alpha1.BridgeProtocolCardDAV, "/contacts/")), time.Unix(200, 0))
	// A later bridge that claims a path under one already served, and one of
	// its own.
	late := bridgeRoute(t, bridgeProfile("other-dav", "other-app",
		deviceWay("way", gentianov1alpha1.BridgeProtocolWebDAV, "/remote.php/dav/files/shared/", "/other/")), time.Unix(300, 0))

	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	// Listed in another order than they were made.
	entries, err := componentRouteTableEntries(context.Background(),
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(late, contacts, files).Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries for one host: %+v", len(entries), entries)
	}
	table, err := bouncerRouteTable(nil, entries)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := bouncer.ParseTable([]byte(table))
	if err != nil {
		t.Fatalf("the bouncer refuses the table: %v\n%s", err, table)
	}
	door := parsed.Match("sync.acme.k.example")
	if door == nil || door.AuthMode != bouncer.AuthModeBearer || door.Relation != "can_enter" || door.Object != "tenant:acme" ||
		door.Audience != "https://sync.acme.k.example" || door.Realm != "acme" || door.ExchangeScope != "" {
		t.Fatalf("the door: %+v\n%s", door, table)
	}
	got := map[string]string{}
	for _, p := range door.Paths {
		got[p.Prefix] = p.Relation + " " + p.Object + " " + p.ExchangeScope
	}
	want := map[string]string{
		"/remote.php/dav/addressbooks": "can_use app:acme/nextcloud-base-ce bridge-nextcloud-base-ce",
		"/remote.php/dav/calendars":    "can_use app:acme/nextcloud-base-ce bridge-nextcloud-base-ce",
		"/remote.php/dav/files":        "can_use app:acme/nextcloud-base-ce bridge-nextcloud-base-ce",
		"/remote.php/dav/principals":   "can_use app:acme/nextcloud-base-ce bridge-nextcloud-base-ce",
		"/contacts":                    "can_use app:acme/odoo-base-ce bridge-odoo-base-ce",
		// The path under one the first bridge serves stays the first's.
		"/other": "can_use app:acme/other-app bridge-other-app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
}

// A bridge's route on a host that has a session changes nothing of that
// host: no path, no exchange scope, no question.
func TestABridgesRouteOnASessionHostIsNotRead(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "files", "tenant-acme"
	web := buildExposureRoute(comp, "acme", "files-web", "sync.acme.k.example", bridgeZone, &gentianov1alpha1.ExposureSpec{
		Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "sync",
		Backend: gentianov1alpha1.BackendRef{Service: "files", Port: 8080},
	}, routeAuthz{relation: "can_use", object: "app:acme/files"}, "k.example", nil)
	web.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))
	bridge := bridgeRoute(t, nextcloudBridge(), time.Unix(100, 0))

	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	entries, err := componentRouteTableEntries(context.Background(),
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(bridge, web).Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%+v", entries)
	}
	if e := entries[0]; e.AuthMode != "oidc" || e.Object != "app:acme/files" || e.ExchangeScope != "" || len(e.Paths) != 0 {
		t.Fatalf("the session host took something of the bridge: %+v", e)
	}
}

func TestPathsOverlapOnSegmentBoundaries(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"/dav", "/dav", true}, {"/dav/", "/dav", true}, {"/dav/files", "/dav", true}, {"/dav", "/dav/files/", true},
		{"/dav", "/davx", false}, {"/dav/files", "/dav/filesystem", false}, {"/a/b", "/a/c", false},
	} {
		if got := pathsOverlap(c.a, c.b); got != c.want {
			t.Errorf("%s and %s: %v", c.a, c.b, got)
		}
	}
}

// The door's label is kept from every app: a component that took it would
// stand where a tenant's sync programs are pointed.
func TestTheDoorsLabelIsKept(t *testing.T) {
	if hostnames.Kept(BridgeDoorLabel) == nil {
		t.Fatalf("%s is not a kept label", BridgeDoorLabel)
	}
}

// A bridge is served while the app it connects is installed and its paths
// are nobody else's at the door, and its route and policy go when it is not.
func TestABridgeIsServedWhileItsAppIsInstalledAndItsPathsAreFree(t *testing.T) {
	ctx := context.Background()
	scheme := perimeterScheme()
	profile, tenant := nextcloudBridge(), acmeTenantFixture()
	comp := componentFor(profile.Name, "tenant-"+tenant.Name)
	comp.Spec.Class = gentianov1alpha1.ComponentClassBridge
	comp.Spec.ProfileRef.Name = profile.Name
	app := componentFor("nextcloud", comp.Namespace)
	app.Spec.ProfileRef.Name = "nextcloud-base-ce"

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi", Cluster: "c1"}
	zone := r.zoneOf(tenant)
	entry, door := bridgeEntryOf(profile), bridgeDoorOf(tenant, profile)
	host := exposureHost(zone, comp, entry)
	reason := func() string {
		cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionBridge)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}

	// The app is not installed.
	ok, err := r.bridgeServable(ctx, comp, profile, host)
	if err != nil || ok || reason() != "AppNotInstalled" {
		t.Fatalf("without the app: %v %v %q", ok, err, reason())
	}

	// Installed: the bridge is served, with the door on its route.
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	ok, err = r.bridgeServable(ctx, comp, profile, host)
	if err != nil || !ok || reason() != "Connected" {
		t.Fatalf("with the app: %v %v %q", ok, err, reason())
	}
	if err := r.ensureTokenEntry(ctx, comp, tenant, profile, zone, entry, door, true, false); err != nil {
		t.Fatal(err)
	}
	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{Namespace: comp.Namespace, Name: tokenEntryRouteName(comp.Name, entry.Name)}
	if err := c.Get(ctx, key, route); err != nil {
		t.Fatal(err)
	}
	if route.Annotations[bouncerObjectAnnotation] != "app:acme/nextcloud-base-ce" || route.Annotations[bouncerDoorPathsAnnotation] == "" ||
		route.Annotations[bouncerExchangeScopeAnnotation] != "bridge-nextcloud-base-ce" || route.Labels[bouncerRouteLabel] != "true" {
		t.Fatalf("the route says %v %v", route.Annotations, route.Labels)
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(securityPolicyGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: tokenEntryPolicyName(comp.Name)}, policy); err != nil {
		t.Fatalf("no policy in front of the bridge's route: %v", err)
	}

	// Another bridge that wants a path under one of this one's is held, and
	// this one is not disturbed by it.
	other := bridgeProfile("other-dav", "nextcloud-base-ce",
		deviceWay("way", gentianov1alpha1.BridgeProtocolWebDAV, "/remote.php/dav/files/shared/"))
	otherComp := componentFor(other.Name, comp.Namespace)
	ok, err = r.bridgeServable(ctx, otherComp, other, host)
	cond := apimeta.FindStatusCondition(otherComp.Status.Conditions, ConditionBridge)
	if err != nil || ok || cond == nil || cond.Reason != "PathTaken" || !strings.Contains(cond.Message, comp.Name) {
		t.Fatalf("a path that is taken: %v %v %+v", ok, err, cond)
	}
	if ok, err = r.bridgeServable(ctx, comp, profile, host); err != nil || !ok {
		t.Fatalf("the bridge that serves the path: %v %v", ok, err)
	}

	// The app is removed: the bridge is not served, and nothing of it stays.
	if err := c.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
	if ok, err = r.bridgeServable(ctx, comp, profile, host); err != nil || ok || reason() != "AppNotInstalled" {
		t.Fatalf("after the app went: %v %v %q", ok, err, reason())
	}
	if err := r.ensureTokenEntry(ctx, comp, tenant, profile, zone, entry, door, false, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, route); err == nil {
		t.Fatal("the route outlived the bridge")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: tokenEntryPolicyName(comp.Name)}, policy); err == nil {
		t.Fatal("the policy outlived the bridge")
	}

	// A component that is no bridge carries no such condition.
	plain := componentFor("wiki", comp.Namespace)
	plain.Status.Conditions = []metav1.Condition{{Type: ConditionBridge, Status: metav1.ConditionTrue, Reason: "Connected"}}
	if ok, err = r.bridgeServable(ctx, plain, tokenEntryProfile(), ""); err != nil || ok || apimeta.FindStatusCondition(plain.Status.Conditions, ConditionBridge) != nil {
		t.Fatalf("no bridge: %v %v %+v", ok, err, plain.Status.Conditions)
	}
}

// The realm holds a client for a bridge's programs while the bridge is
// approved, made out to the door and to the edge's exchange client, and holds
// none once the approval is gone.
func TestABridgesRealmClientComesAndGoesWithItsApproval(t *testing.T) {
	ctx := context.Background()
	scheme := perimeterScheme()
	profile, tenant := nextcloudBridge(), acmeTenantFixture()
	comp := componentFor(profile.Name, "tenant-"+tenant.Name)
	comp.UID = "uid-bridge"
	comp.Spec.Class = gentianov1alpha1.ComponentClassBridge
	comp.Spec.ProfileRef.Name = profile.Name
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi", Cluster: "c1"}
	zone := r.zoneOf(tenant)

	client := &unstructured.Unstructured{}
	client.SetAPIVersion("openidclient.keycloak.crossplane.io/v1alpha1")
	client.SetKind("Client")
	key := types.NamespacedName{Name: tenant.Name + "-" + comp.Name + "-token-entry-client"}
	held := func() bool {
		t.Helper()
		if _, _, err := r.ensureBridgeClient(ctx, comp, profile, tenant, zone, pullSecrets{}, nil, nil); err != nil {
			t.Fatal(err)
		}
		return c.Get(ctx, key, client) == nil
	}

	if held() {
		t.Fatal("a client for a bridge nobody approved")
	}
	comp.Spec.Exposures = []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "files", Kind: gentianov1alpha1.ExposureKindTokenEntry, Owner: "u-tom",
		ReviewAt: metav1.NewTime(time.Now().Add(24 * time.Hour)),
	}}
	if !held() {
		t.Fatal("no client for an approved bridge")
	}
	fp, _, _ := unstructured.NestedMap(client.Object, "spec", "forProvider")
	if fp["clientId"] != "token-entry-nextcloud-dav" || fp["realmId"] != "acme" || fp["accessType"] != "PUBLIC" {
		t.Fatalf("the client: %v", fp)
	}
	if got, _, _ := unstructured.NestedStringSlice(client.Object, "spec", "forProvider", "validRedirectUris"); !reflect.DeepEqual(got, bridgeEntryOf(profile).ClientRedirectURIs) {
		t.Fatalf("redirects %v", got)
	}
	// Its two audience mappers, by the names the render gives them.
	audiences := map[string]bool{}
	for _, suffix := range []string{"token-entry-audience", "token-entry-audience-" + bouncer.ExchangeClientID} {
		mapper := &unstructured.Unstructured{}
		mapper.SetAPIVersion("client.keycloak.crossplane.io/v1alpha1")
		mapper.SetKind("ProtocolMapper")
		if err := c.Get(ctx, types.NamespacedName{Name: tenant.Name + "-" + comp.Name + "-" + suffix}, mapper); err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
		aud, _, _ := unstructured.NestedString(mapper.Object, "spec", "forProvider", "config", "included.custom.audience")
		audiences[aud] = true
	}
	if len(audiences) != 2 || !audiences["https://sync.acme.k.example"] || !audiences[bouncer.ExchangeClientID] {
		t.Fatalf("a token of the client is made out to %v", audiences)
	}

	// Approved as something else is not approved.
	comp.Spec.Exposures[0].Kind = gentianov1alpha1.ExposureKindPublic
	if held() {
		t.Fatal("the client outlived the approval")
	}
}

// Each way of a bridge is approved on its own, and the door serves the paths
// of the ways approved and no other: a path two ways share is served while
// either is.
func TestTheDoorServesTheApprovedWaysOfABridgeAndNoOther(t *testing.T) {
	profile := nextcloudBridge()
	comp := componentFor(profile.Name, "tenant-acme")
	now := time.Now()
	approve := func(ways ...string) {
		comp.Spec.Exposures = nil
		for _, w := range ways {
			comp.Spec.Exposures = append(comp.Spec.Exposures, gentianov1alpha1.ExposureEnablement{
				ExposureName: w, Kind: gentianov1alpha1.ExposureKindTokenEntry, Owner: "u-tom",
				ReviewAt: metav1.NewTime(now.Add(24 * time.Hour)),
			})
		}
	}

	if got := approvedBridgeWays(comp, profile, false, now); len(got) != 0 || servedBridgeEntry(profile, got) != nil {
		t.Fatalf("approved before anyone approved: %v", got)
	}
	approve("calendars")
	got := approvedBridgeWays(comp, profile, false, now)
	served := servedBridgeEntry(profile, got)
	if !reflect.DeepEqual(got, []string{"calendars"}) || served == nil ||
		!reflect.DeepEqual(served.Paths, []string{"/remote.php/dav/calendars", "/remote.php/dav/principals"}) {
		t.Fatalf("one way approved: %v, served %+v", got, served)
	}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "acme"
	if door := bridgeDoorOf(tenant, profile, got...); !reflect.DeepEqual(door.paths, served.Paths) {
		t.Fatalf("the bouncer is told %v", door.paths)
	}
	// Approved as something else, or under the name of the whole route, is
	// not a way approved.
	approve("bridge")
	comp.Spec.Exposures = append(comp.Spec.Exposures, gentianov1alpha1.ExposureEnablement{
		ExposureName: "files", Kind: gentianov1alpha1.ExposureKindPublic, Owner: "u-tom",
	})
	if got := approvedBridgeWays(comp, profile, false, now); len(got) != 0 {
		t.Fatalf("approved as %v", got)
	}
	// The tenant waives approvals, as the cluster allows: every way.
	if got := approvedBridgeWays(comp, profile, true, now); len(got) != 3 {
		t.Fatalf("waived: %v", got)
	}
	// Each way is one entry an approver is asked about, with its own paths.
	entries := profile.BridgeWayEntries()
	if len(entries) != 3 || entries[0].Name != "files" || !reflect.DeepEqual(entries[0].Paths, []string{"/remote.php/dav/files"}) ||
		entries[0].RequestKind() != gentianov1alpha1.ExposureKindTokenEntry {
		t.Fatalf("entries %+v", entries)
	}
}

// Only a way from a program on a device to the app, in a protocol the app
// speaks over HTTP, is served at the door. A bridge with no such way has no
// entry there.
func TestOnlyADeviceWayToTheAppIsServedAtTheDoor(t *testing.T) {
	agents := gentianov1alpha1.BridgeWay{
		Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents, Callee: gentianov1alpha1.BridgeCalleeApp,
		Protocol: gentianov1alpha1.BridgeProtocolMCP, SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson,
	}
	mcp := deviceWay("mcp", gentianov1alpha1.BridgeProtocolMCP, "/mcp")
	for name, profile := range map[string]*gentianov1alpha1.ComponentProfile{
		"an agent bridge":   bridgeProfile("agent-bridge", "odoo", agents),
		"mcp from a device": bridgeProfile("device-mcp", "odoo", mcp),
	} {
		if bridgeEntryOf(profile) != nil || len(profile.BridgeWayEntries()) != 0 {
			t.Errorf("%s has an entry at the door", name)
		}
	}
	mixed := bridgeProfile("mixed", "nextcloud-base-ce", agents, deviceWay("files", gentianov1alpha1.BridgeProtocolWebDAV, "/remote.php/dav/files/"))
	if e := bridgeEntryOf(mixed); e == nil || !reflect.DeepEqual(e.Paths, []string{"/remote.php/dav/files"}) || len(mixed.BridgeWayEntries()) != 1 {
		t.Fatalf("a bridge with one door way: %+v", e)
	}
}
