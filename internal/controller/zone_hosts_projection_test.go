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
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func exposeAt(name, sub string, surface gentianov1alpha1.SurfaceKind) gentianov1alpha1.ExposureSpec {
	return gentianov1alpha1.ExposureSpec{
		Name: name, Surface: surface, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: sub,
		Backend: gentianov1alpha1.BackendRef{Service: "s", Port: 80},
	}
}

func zoneHostsOf(t *testing.T, r *TileProjectionReconciler, tenant string) []string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: zoneHostsConfigMapName(tenant), Namespace: layout.Namespace(layout.Control),
	}
	if err := r.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("no projection for %s: %v", tenant, err)
	}
	if cm.Data[zoneHostsKey] == "" {
		return nil
	}
	return strings.Split(cm.Data[zoneHostsKey], "\n")
}

// The zone's Keycloak client admits a redirect only to a host it lists, and
// that list was written in the Composition by hand. A component declaring a
// subDomain of its own got "Invalid parameter: redirect_uri" on a page naming
// neither the component nor a list. The operator knows every host, because it
// composes the routes, so it projects them.
func TestZoneHostsFollowTheComponentsAndNotAList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	wiki := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "wiki"}}
	wiki.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		exposeAt("web", "wiki", gentianov1alpha1.SurfaceGateway),
		// A second entry on the same host must not double it.
		exposeAt("api", "wiki", gentianov1alpha1.SurfaceGateway),
		// A perimeter surface is published through its own proxy with its own
		// credential and never reaches the zone's client, so listing it would
		// widen that client for a host the zone does not serve.
		exposeAt("share", "public", gentianov1alpha1.SurfacePerimeter),
	}
	// No subDomain: the route is served on the component's own name.
	unnamed := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "notes"}}
	unnamed.Spec.Expose = []gentianov1alpha1.ExposureSpec{exposeAt("web", "", gentianov1alpha1.SurfaceGateway)}

	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: tenant.NamespaceName(),
		}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	// A service runs in no tenant's namespace and is in nobody's zone.
	svc := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "system-data"}}
	svc.Spec.ProfileRef.Name = "wiki"
	svc.Spec.Class = gentianov1alpha1.ComponentClassService

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), wiki, unnamed, comp("wiki"), comp("notes"), svc,
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := zoneHostsOf(t, r, tenant.Name)
	want := []string{"notes", "wiki"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("hosts = %v, want %v: sorted, deduplicated, gateway entries only", got, want)
	}
}

// A tenant whose components declare no host still gets a projection. Its
// absence would be indistinguishable from "the operator has not looked yet",
// and the Composition would have no way to tell them apart.
func TestATenantWithNoComponentHostsStillGetsAProjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := zoneHostsOf(t, r, tenant.Name); len(got) != 0 {
		t.Fatalf("hosts = %v, want none", got)
	}
}

func exchangeProfilesOf(t *testing.T, r *TileProjectionReconciler, tenant string) []string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: zoneHostsConfigMapName(tenant), Namespace: layout.Namespace(layout.Control),
	}
	if err := r.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("no projection for %s: %v", tenant, err)
	}
	if cm.Data[zoneExchangeProfilesKey] == "" {
		return nil
	}
	return strings.Split(cm.Data[zoneExchangeProfilesKey], "\n")
}

// The tenant's realm holds one scope per app that is handed an exchanged
// token, and the Composition makes them from this list. It follows the
// Components, as the routes that ask for the scopes do: an app is on it when
// an exposure of its profile says exchangeToken, whether a tenant installed
// it or the platform placed it, and an app that does not say so is not.
func TestExchangeProfilesFollowTheComponentsThatAsk(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	profile := func(name string, expose ...gentianov1alpha1.ExposureSpec) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.Expose = expose
		return p
	}
	asking := func(name string) gentianov1alpha1.ExposureSpec {
		e := exposeAt(name, "", gentianov1alpha1.SurfaceGateway)
		e.ExchangeToken = true
		return e
	}
	comp := func(name, profileName, namespace string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		c.Spec.ProfileRef.Name = profileName
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		// One entry of several is enough: the scope is the app's.
		profile("wiki", exposeAt("web", "wiki", gentianov1alpha1.SurfaceGateway), asking("api")),
		profile("notes", asking("web")),
		// Does not ask, and so has no scope: its backend goes on reading
		// the identity headers.
		profile("plain", exposeAt("web", "", gentianov1alpha1.SurfaceGateway)),
		// Asks, and no component of this tenant is of it.
		profile("elsewhere", asking("web")),
		comp("wiki", "wiki", tenant.NamespaceName()),
		// Named after the profile, not the component: the route builder
		// names the scope AppTokenScope(profile), and two components of one
		// profile are one audience.
		comp("notes-a", "notes", tenant.NamespaceName()),
		comp("notes-b", "notes", tenant.NamespaceName()),
		comp("plain", "plain", tenant.NamespaceName()),
		// A component in nobody's namespace is in nobody's realm.
		comp("elsewhere", "elsewhere", "system-data"),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := exchangeProfilesOf(t, r, tenant.Name)
	want := []string{"notes", "wiki"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("exchange profiles = %v, want %v: sorted, one per profile, only the ones that ask", got, want)
	}

	// The profile stops asking, and the next projection says so: the list
	// is what takes the scope out of the realm's exchange client.
	wiki := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wiki"}, wiki); err != nil {
		t.Fatal(err)
	}
	for i := range wiki.Spec.Expose {
		wiki.Spec.Expose[i].ExchangeToken = false
	}
	if err := c.Update(context.Background(), wiki); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exchangeProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "notes" {
		t.Fatalf("exchange profiles = %v after wiki stopped asking, want notes alone", got)
	}
}

// An installed agent bridge's app is handed tokens by the components that
// vouch for people, so it has a scope whether or not an entry of its own
// asks: by the connected profile's name, and only for a bridge that is
// called.
func TestABridgedAppHasAnExchangeScope(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	agentWay := func(endpoint *gentianov1alpha1.ContractEndpoint) []gentianov1alpha1.BridgeWay {
		return []gentianov1alpha1.BridgeWay{{Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
			Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolMCP,
			SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson, Endpoint: endpoint}}
	}
	app := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "ledger"}}
	bridge := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "ledger-adapter"}}
	bridge.Spec.Bridge = &gentianov1alpha1.BridgeSpec{
		App:  &gentianov1alpha1.BridgedApp{Profile: "ledger"},
		Ways: agentWay(&gentianov1alpha1.ContractEndpoint{Service: "ledger-adapter", Port: 8765, Path: "/mcp"}),
	}
	// Connects nothing of the tenant: calls through it are for itself.
	partner := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "store-adapter"}}
	partner.Spec.Bridge = &gentianov1alpha1.BridgeSpec{
		Ways: agentWay(&gentianov1alpha1.ContractEndpoint{Service: "store-adapter", Port: 8765, Path: "/mcp"}),
	}
	partner.Spec.Bridge.Ways[0].Callee = gentianov1alpha1.BridgeCalleePartner
	quiet := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "quiet-adapter"}}
	quiet.Spec.Bridge = &gentianov1alpha1.BridgeSpec{App: &gentianov1alpha1.BridgedApp{Profile: "quiet"}, Ways: agentWay(nil)}
	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.NamespaceName()}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), app, bridge, partner, quiet,
		comp("ledger"), comp("ledger-adapter"), comp("store-adapter"), comp("quiet-adapter"),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exchangeProfilesOf(t, r, tenant.Name); len(got) != 2 || got[0] != "ledger" || got[1] != "store-adapter" {
		t.Fatalf("exchange profiles = %v, want the connected app and the bridge that connects none", got)
	}
}

func vouchingProfilesOf(t *testing.T, r *TileProjectionReconciler, tenant string) []string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: zoneHostsConfigMapName(tenant), Namespace: layout.Namespace(layout.Control),
	}
	if err := r.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("no projection for %s: %v", tenant, err)
	}
	if cm.Data[zoneVouchingProfilesKey] == "" {
		return nil
	}
	return strings.Split(cm.Data[zoneVouchingProfilesKey], "\n")
}

// The tenant's realm believes one issuer per component that vouches for
// people, and the Composition makes them from this list: the profile's name
// and where its keys are published. It follows the Components, as the
// exchange profiles do, and says the path out in full so that nothing
// downstream has to know the default.
//
// The profiles here are the ones the tenant-default render fixture's
// projection lists, and what is projected for them is compared with that
// fixture: the lines the Composition is tested on are lines this writes.
func TestVouchingProfilesFollowTheComponentsThatVouch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	vouching := func(name, service string, port int32, path string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
			Vouching: &gentianov1alpha1.VouchingRequirement{
				Keys: &gentianov1alpha1.VouchingKeys{Service: service, Port: port, Path: path},
			},
		}}
		return p
	}
	comp := func(name, profileName, namespace string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		c.Spec.ProfileRef.Name = profileName
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	// Requires other things and not this.
	plain := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	plain.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		// No path: a profile no API server defaulted. The default is
		// written out.
		vouching("scribe", "scribe", 8080, ""),
		// Exchanges as well: the word that says so follows its line.
		exchanging(vouching("steward", "steward-keys", 8443, "/keys/current.json")),
		plain,
		// Exchanges and names no keys: the line says there are none.
		keyless("warden"),
		// Vouches, and no component of this tenant is of it.
		vouching("elsewhere", "elsewhere", 8080, ""),
		// Two components of one profile are one issuer with one set of keys.
		comp("scribe-a", "scribe", tenant.NamespaceName()),
		comp("scribe-b", "scribe", tenant.NamespaceName()),
		comp("steward", "steward", tenant.NamespaceName()),
		comp("plain", "plain", tenant.NamespaceName()),
		comp("warden", "warden", tenant.NamespaceName()),
		// A component in nobody's namespace is in nobody's realm.
		comp("elsewhere", "elsewhere", "system-data"),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := vouchingProfilesOf(t, r, tenant.Name)
	want := []string{
		"scribe scribe 8080 " + vouchingKeysDefaultPath,
		"steward steward-keys 8443 /keys/current.json exchange",
		"warden - 0 - exchange",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("vouching profiles = %q, want %q: sorted, one line per profile, only the ones that vouch", got, want)
	}

	// What the Composition is rendered from in the fixture is what this
	// writes, key and lines.
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "tests", "unit", "render", "tenant-default", "required-resources", "zone-hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(raw, fixture); err != nil {
		t.Fatal(err)
	}
	if lines := strings.TrimSpace(fixture.Data[zoneVouchingProfilesKey]); lines != strings.Join(got, "\n") {
		t.Fatalf("the render fixture's %s is\n%s\nand the projection writes\n%s", zoneVouchingProfilesKey, lines, strings.Join(got, "\n"))
	}

	// The profile stops vouching, and the next projection says so.
	steward := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "steward"}, steward); err != nil {
		t.Fatal(err)
	}
	steward.Spec.Requires.Services.Vouching = nil
	if err := c.Update(context.Background(), steward); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 2 || got[0] != want[0] || got[1] != want[2] {
		t.Fatalf("vouching profiles = %q after steward stopped vouching, want the other two", got)
	}
}

// A token for the director is the cluster's administrators', who sign in at
// the platform tenant's realm: the director scopes a profile names are
// written in that tenant's projection, and in no other's.
func TestDirectorScopesAreProjectedOnlyForThePlatformTenant(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	platform := platformTenantFixture()
	acme := acmeTenantFixture()
	syncer := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "syncer"}}
	syncer.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{
			Keys:     &gentianov1alpha1.VouchingKeys{Service: "syncer", Port: 8080},
			Director: []gentianov1alpha1.DirectorScope{gentianov1alpha1.DirectorScopePlansApply, gentianov1alpha1.DirectorScopePlansRead},
		},
	}}
	comp := func(namespace string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "syncer", Namespace: namespace}}
		c.Spec.ProfileRef.Name = "syncer"
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		platform.DeepCopy(), acme.DeepCopy(), syncer,
		comp(platform.NamespaceName()), comp(acme.NamespaceName()),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, platform.Name); len(got) != 1 || got[0] != "syncer syncer 8080 "+vouchingKeysDefaultPath+" plans-apply,plans-read" {
		t.Fatalf("platform: %q", got)
	}
	if got := vouchingProfilesOf(t, r, acme.Name); len(got) != 1 || got[0] != "syncer syncer 8080 "+vouchingKeysDefaultPath {
		t.Fatalf("another tenant was given director scopes: %q", got)
	}
}

// keyless is a profile that exchanges a present person's token and vouches
// for nobody: it names no keys.
func keyless(name string) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{Exchange: true},
	}}
	return p
}

// exchanging says the profile's client also exchanges a person's own token
// (requires.services.vouching.exchange).
func exchanging(p *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.ComponentProfile {
	p.Spec.Requires.Services.Vouching.Exchange = true
	return p
}

// A certified app that calls a home as the person is projected as narrow,
// with the homes granted to it: a binding of its contract marked asPerson
// whose grant is in full. A grant taken back, or a contract it does not call
// as the person, names no home. The homes' scopes are projected beside.
func TestHomesGrantedToAnAppThatCallsThemAsThePerson(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := acmeTenantFixture()
	ns := tenant.NamespaceName()
	odoo := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "odoo"}}
	odoo.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	odoo.Spec.Integrations = []gentianov1alpha1.IntegrationRef{
		{Contract: "calendar-store", AsPerson: true},
		{Contract: "contacts-store", AsPerson: true},
		{Contract: "file-store"},
	}
	odoo.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{Keys: &gentianov1alpha1.VouchingKeys{Service: "odoo-keys", Port: 8069, Path: "/jwks"}},
	}}
	home := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud"}}
	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	binding := func(contract string, granted bool) *gentianov1alpha1.IntegrationBinding {
		ib := &gentianov1alpha1.IntegrationBinding{ObjectMeta: metav1.ObjectMeta{Name: "odoo-" + contract, Namespace: ns}}
		ib.Spec.Contract = contract
		ib.Spec.Consumer.App = "odoo"
		ib.Spec.Provider.App = "nextcloud"
		status := metav1.ConditionFalse
		if granted {
			status = metav1.ConditionTrue
		}
		ib.Status.Conditions = []metav1.Condition{{Type: conditionBindingGranted, Status: status, Reason: "x"}}
		return ib
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&gentianov1alpha1.IntegrationBinding{}).WithObjects(
		tenant.DeepCopy(), odoo, home, comp("odoo"), comp("nextcloud"),
		binding("calendar-store", true),
		// Not granted: no home by it.
		binding("contacts-store", false),
		// Granted, and not called as the person: no home by it either.
		binding("file-store", true),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "odoo odoo-keys 8069 /jwks narrow,home=nextcloud" {
		t.Fatalf("vouching profiles = %q", got)
	}

	// The grant is taken back: the app is still narrow, and has no home.
	ib := &gentianov1alpha1.IntegrationBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "odoo-calendar-store", Namespace: ns}, ib); err != nil {
		t.Fatal(err)
	}
	ib.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := c.Status().Update(context.Background(), ib); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "odoo odoo-keys 8069 /jwks narrow" {
		t.Fatalf("vouching profiles after the grant was taken back = %q", got)
	}
}

// A certified app that names no keys calls its home only while the person is
// present: it is projected with no keys, exchanging and narrow, with the
// homes granted to it. The Composition makes it a client that exchanges and
// no issuer.
func TestAnAppThatCallsItsHomeOnlyWhileThePersonIsPresent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := acmeTenantFixture()
	ns := tenant.NamespaceName()
	editor := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "editor"}}
	editor.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	editor.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "file-store", AsPerson: true}}
	editor.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{Exchange: true},
	}}
	editor.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: "oidc", ExchangeToken: true,
	}}
	home := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud"}}
	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	ib := &gentianov1alpha1.IntegrationBinding{ObjectMeta: metav1.ObjectMeta{Name: "editor-file-store", Namespace: ns}}
	ib.Spec.Contract = "file-store"
	ib.Spec.Consumer.App = "editor"
	ib.Spec.Provider.App = "nextcloud"
	ib.Status.Conditions = []metav1.Condition{{Type: conditionBindingGranted, Status: metav1.ConditionTrue, Reason: "x"}}
	// The Secret the Composition wrote for the app, with a home's scope left
	// from a contract it is no longer granted.
	told := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: VouchingSecretName("editor"), Namespace: ns}}
	told.Data = map[string][]byte{
		VouchingClientIDKey:               []byte("vouch-editor"),
		VouchingHomeKey("calendar-store"): []byte("home-elsewhere"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&gentianov1alpha1.IntegrationBinding{}).WithObjects(
		tenant.DeepCopy(), editor, home, comp("editor"), comp("nextcloud"), ib, told,
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "editor - 0 - exchange,narrow,home=nextcloud" {
		t.Fatalf("vouching profiles = %q", got)
	}

	// The app is told the scope to ask for, by contract, and nothing of a
	// home it is not granted.
	key := types.NamespacedName{Name: told.Name, Namespace: ns}
	if err := c.Get(context.Background(), key, told); err != nil {
		t.Fatal(err)
	}
	if got := string(told.Data["VOUCHING_HOME_FILE_STORE"]); got != "home-nextcloud" {
		t.Fatalf("VOUCHING_HOME_FILE_STORE = %q", got)
	}
	if _, left := told.Data[VouchingHomeKey("calendar-store")]; left || len(told.Data) != 2 {
		t.Fatalf("the Secret holds %v", told.Data)
	}

	// The grant is taken back: the scope's name goes.
	ib.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := c.Status().Update(context.Background(), ib); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, told); err != nil {
		t.Fatal(err)
	}
	if len(told.Data) != 1 {
		t.Fatalf("after the grant was taken back the Secret holds %v", told.Data)
	}
}

// A home's scope is its own sign-in's token: projected only while some
// component is granted the home, from the home's OIDC pack.
func TestHomeTokenScopeIsNamedForTheHome(t *testing.T) {
	if got := HomeTokenScope("nextcloud-base-ce"); got != "home-nextcloud-base-ce" {
		t.Fatalf("HomeTokenScope = %q", got)
	}
}

// The Secret the Composition wrote once follows the profile: a component
// that starts to publish keys is told its issuer, one that stops is no
// longer, and the client secret beside it is never touched. A Secret not
// made yet is left to the Composition.
func TestTheVouchingIssuerFollowsTheProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	ns := tenant.NamespaceName()
	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	secret := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: VouchingSecretName(name), Namespace: ns}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	signer := keyless("signer")
	signer.Spec.Requires.Services.Vouching.Keys = &gentianov1alpha1.VouchingKeys{Service: "signer", Port: 8080}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		// Wrote no issuer when it was made: the profile named no keys then.
		signer, comp("signer"),
		secret("signer", map[string]string{VouchingClientSecretKey: "s1", VouchingClientIDKey: "vouch-signer"}),
		// Named keys once and no longer does.
		keyless("stopped"), comp("stopped"),
		secret("stopped", map[string]string{VouchingClientSecretKey: "s2", VouchingIssuerKey: VouchingIssuer(ns, "stopped")}),
		// Its Secret is not made yet.
		keyless("new"), comp("new"),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := func(name string) map[string][]byte {
		s := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: VouchingSecretName(name)}, s); err != nil {
			t.Fatal(err)
		}
		return s.Data
	}
	got := read("signer")
	if string(got[VouchingIssuerKey]) != VouchingIssuer(ns, "signer") || string(got[VouchingClientSecretKey]) != "s1" || string(got[VouchingClientIDKey]) != "vouch-signer" {
		t.Fatalf("signer's Secret = %q", got)
	}
	got = read("stopped")
	if _, ok := got[VouchingIssuerKey]; ok || string(got[VouchingClientSecretKey]) != "s2" {
		t.Fatalf("stopped's Secret = %q", got)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: VouchingSecretName("new")}, &corev1.Secret{}); !errors.IsNotFound(err) {
		t.Fatalf("a Secret not made yet was made: %v", err)
	}
}

// The porter, which the platform places in every tenant, calls the tenant's
// file home as the person at its address with nobody asked: no binding is
// granted to it, and it is projected as a certified app that is present-only
// is -- no keys, exchanging, narrow, with the home -- and told the scope.
// Without a file home it is given none.
func TestThePlacedPorterIsGivenTheTenantsFileHome(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := acmeTenantFixture()
	ns := tenant.NamespaceName()
	porter := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PorterProfile}}
	porter.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	porter.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: "every"}
	porter.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "file-store", AsPerson: true}}
	porter.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{Exchange: true},
	}}
	porter.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: "oidc", SubDomain: "files", ExchangeToken: true,
	}}
	home := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud"}}
	home.Spec.Provides = []gentianov1alpha1.ContractRef{{Name: "file-store"}}
	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	told := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: VouchingSecretName(porter.Name), Namespace: ns}}
	told.Data = map[string][]byte{VouchingClientIDKey: []byte("vouch-porter")}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), porter, home, comp(porter.Name), comp("nextcloud"), told,
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "porter - 0 - exchange,narrow,home=nextcloud" {
		t.Fatalf("vouching profiles = %q", got)
	}
	key := types.NamespacedName{Name: told.Name, Namespace: ns}
	if err := c.Get(context.Background(), key, told); err != nil {
		t.Fatal(err)
	}
	if got := string(told.Data["VOUCHING_HOME_FILE_STORE"]); got != "home-nextcloud" {
		t.Fatalf("VOUCHING_HOME_FILE_STORE = %q", got)
	}

	// The file home is removed: the porter keeps its client and no scope.
	if err := c.Delete(context.Background(), comp("nextcloud")); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vouchingProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "porter - 0 - exchange,narrow" {
		t.Fatalf("without a file home: %q", got)
	}
	if err := c.Get(context.Background(), key, told); err != nil {
		t.Fatal(err)
	}
	if len(told.Data) != 1 {
		t.Fatalf("without a file home the Secret holds %v", told.Data)
	}
}
