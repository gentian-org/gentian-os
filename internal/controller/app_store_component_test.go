/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// renderShippedProfile renders one ComponentProfile of the operator chart the
// way the installer does, and reads it strictly: a field the type does not
// have fails here, not as a pruned field on a cluster.
func renderShippedProfile(t *testing.T, template string, set ...string) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is needed to render the operator chart's profiles: %v", err)
	}
	args := []string{"template", "gentian-os", filepath.Join("..", "..", "charts", "gentian-os"), "-s", "templates/" + template}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", template, err, stderr.String())
	}
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.UnmarshalStrict(out, profile); err != nil {
		t.Fatalf("%s is not a ComponentProfile: %v", template, err)
	}
	return profile
}

func shippedAppStoreProfile(t *testing.T) *gentianov1alpha1.ComponentProfile {
	return renderShippedProfile(t, "componentprofile-app-store.yaml",
		"appStore.enabled=true", "appStore.chart.version=0.1.0-develop.9c4d6bb")
}

// clusterClaim is a Cluster claim where the operator reads it, naming a store
// or none.
func clusterClaim(storeURL string) *unstructured.Unstructured {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(clusterClaimGVK)
	claim.SetName("cluster")
	claim.SetNamespace(clusterConfigNamespace)
	_ = unstructured.SetNestedField(claim.Object, "k.example", "spec", "kernelDomain")
	if storeURL != "" {
		_ = unstructured.SetNestedField(claim.Object, storeURL, "spec", "catalogue", "storeUrl")
	}
	return claim
}

func appStoreScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		gentianov1alpha1.AddToScheme, gatewayv1.Install, corev1.AddToScheme, networkingv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// The profile the chart ships is the one the app's README asks for: the API
// behind /api, the store's sign-in callback and the two probes, with the
// zone's token forwarded; everything else to the bundle; one host, store; and
// a tile for the people who may install apps in the tenant.
func TestTheAppStoreProfileRoutesAsTheAppExpects(t *testing.T) {
	p := shippedAppStoreProfile(t)
	if p.Name != "app-store" || p.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform {
		t.Fatalf("profile %s, tier %s", p.Name, p.Spec.TrustTier)
	}
	if !p.Spec.DefaultWhereStoreOffered || p.Spec.DefaultForTenants || p.Spec.DefaultForPlatform {
		t.Fatalf("placement: whereStoreOffered=%v forTenants=%v forPlatform=%v",
			p.Spec.DefaultWhereStoreOffered, p.Spec.DefaultForTenants, p.Spec.DefaultForPlatform)
	}
	if c := p.Spec.Package.Chart; c == nil || c.Name != "app-store" || c.Version != "0.1.0-develop.9c4d6bb" ||
		c.Repository != "oci://ghcr.io/gentian-org/charts" {
		t.Fatalf("chart = %+v", p.Spec.Package.Chart)
	}
	if len(p.Spec.Expose) != 2 {
		t.Fatalf("exposures = %d, want the API and the bundle", len(p.Spec.Expose))
	}
	api, web := p.Spec.Expose[0], p.Spec.Expose[1]
	if api.Name != "api" || !api.ForwardToken || api.AuthMode != gentianov1alpha1.AuthModeOIDC ||
		api.SubDomain != "store" || api.Backend.Service != "app-store-api" || api.Backend.Port != 8000 {
		t.Fatalf("api exposure = %+v", api)
	}
	if got := fmt.Sprint(api.Paths); got != "[/api /oauth/callback /healthz /readyz]" {
		t.Fatalf("api paths = %s", got)
	}
	if web.Name != "web" || web.ForwardToken || web.AuthMode != gentianov1alpha1.AuthModeOIDC ||
		web.SubDomain != "store" || len(web.Paths) != 0 || web.Backend.Service != "app-store-web" || web.Backend.Port != 8080 {
		t.Fatalf("web exposure = %+v", web)
	}
	if api.Tile != nil {
		t.Fatal("the API entry carries a tile")
	}
	tile := web.Tile
	if tile == nil || tile.DisplayName != "App Store" || tile.Relation != "can_install_app" ||
		tile.Object != gentianov1alpha1.TileObjectTenant {
		t.Fatalf("tile = %+v", tile)
	}
	// The glyph is the desktop's own "store".
	var glyphs struct {
		Tiles map[string]struct {
			DataURI string `json:"dataUri"`
		} `json:"tiles"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "tiles", "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &glyphs); err != nil {
		t.Fatal(err)
	}
	if tile.Logo == "" || tile.Logo != glyphs.Tiles["store"].DataURI {
		t.Fatal("the tile's logo is not the store glyph of internal/tiles/catalogue.json")
	}
	// The callback's rule is a path prefix on segment boundaries, like every
	// rule of an exposure: /oauth/callback and what is below it. The edge's
	// own callback is under /oauth2/, which every route behind a session
	// carries and the session filter answers itself.
	route := buildExposureRoute(&gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "app-store", Namespace: "tenant-acme"}},
		"app-store-api", "store.acme.k.example", edgeZone{}, &api, routeAuthz{relation: "can_install_app", object: "tenant:acme"}, "k.example", nil)
	var prefixes []string
	for _, rule := range route.Spec.Rules {
		for _, m := range rule.Matches {
			if m.Path == nil || m.Path.Type == nil || *m.Path.Type != gatewayv1.PathMatchPathPrefix {
				t.Fatalf("a rule of the API route is not a path prefix: %+v", m.Path)
			}
			prefixes = append(prefixes, *m.Path.Value)
		}
		for _, b := range rule.BackendRefs {
			if string(b.Name) != "app-store-api" {
				t.Fatalf("an API rule routes to %s", b.Name)
			}
		}
	}
	if got := fmt.Sprint(prefixes); got != "[/api /oauth/callback /healthz /readyz /oauth2/]" {
		t.Fatalf("API route prefixes = %s", got)
	}
}

// Both routes of the App Store app ask the install right on the tenant, which
// is what its tile asks: a member who knows the host is refused at the edge.
// The consoles are unchanged -- their tile asks can_administer and their
// routes still ask can_enter.
func TestTheAppStoreRoutesAskTheInstallRight(t *testing.T) {
	tenant := acmeTenantFixture()
	comp := tileFixtureComponent("app-store", "tenant-acme", "app-store")
	profile := shippedAppStoreProfile(t)
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		got := exposureAuthz(tenant, comp, profile, e.ForwardToken)
		if got.relation != "can_install_app" || got.object != "tenant:acme" {
			t.Fatalf("%s asks %s on %s", e.Name, got.relation, got.object)
		}
	}
	console := renderShippedProfile(t, "componentprofile-admin-console.yaml", "adminConsole.enabled=true")
	if got := exposureAuthz(tenant, comp, console, false); got.relation != "can_enter" || got.object != "tenant:acme" {
		t.Fatalf("the administration console asks %s on %s", got.relation, got.object)
	}

	// What the bouncer is told: one entry for the host, with that question
	// and the token forwarded.
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	zone := r.zoneOf(tenant)
	var routes []client.Object
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		routes = append(routes, buildExposureRoute(comp, comp.Name+"-"+e.Name, exposureHost(zone, comp, e), zone, e,
			exposureAuthz(tenant, comp, profile, e.ForwardToken), r.KernelDomain, nil))
	}
	c := fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(routes...).Build()
	table, err := componentRouteTableEntries(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 {
		t.Fatalf("route table = %+v, want one host", table)
	}
	if e := table[0]; e.Host != "store.acme.k.example" || e.Relation != "can_install_app" || e.Object != "tenant:acme" ||
		!e.ForwardToken || e.AuthMode != "oidc" {
		t.Fatalf("route table entry = %+v", e)
	}
}

// The tile is asked on the tenant with the install right, so the usher lists
// it for the people who hold that and for nobody else.
func TestTheAppStoreTileAsksTheInstallRightOnTheTenant(t *testing.T) {
	got := project(t, tileFixtureTenant("acme"),
		tileFixtureComponent("app-store", "tenant-acme", "app-store"),
		shippedAppStoreProfile(t),
		tileFixtureRoute("app-store-web", "tenant-acme", "store.acme.k.example"))
	if len(got) != 1 {
		t.Fatalf("tiles = %v", tileNames(got))
	}
	if got[0].Name != "acme/app-store/web" || got[0].URL != "https://store.acme.k.example/" ||
		got[0].Object != "tenant:acme" || fmt.Sprint(got[0].AnyOf) != "[can_install_app]" || got[0].DisplayName != "App Store" {
		t.Fatalf("tile = %+v", got[0])
	}
}

// What the platform sends the app's chart, for a tenant of a multi-tenancy
// cluster and for the user tenant of a single-tenancy one: every key the
// chart reads, and a host whose first label is store.
func appStoreValues(t *testing.T, tenancyMode, tenantName string, reporting bool, claim *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	t.Setenv("GENTIAN_NS_CONTROL", "kernel-control")
	b := fake.NewClientBuilder().WithScheme(appStoreScheme(t))
	if claim != nil {
		b = b.WithObjects(claim)
	}
	r := &ComponentReconciler{Client: b.Build(), KernelDomain: "k.example", KernelRealm: "kernel",
		TenancyMode: tenancyMode, Cluster: "c1", LicenceReporting: reporting}
	profile := shippedAppStoreProfile(t)
	tenant := tileFixtureTenant(tenantName)
	comp := tileFixtureComponent("app-store", "tenant-"+tenantName, "app-store")
	zone := r.zoneOf(tenant)
	values := map[string]interface{}{}
	if err := decodeJSONObject(profile.Spec.Package.ExtraValues.Raw, &values); err != nil {
		t.Fatal(err)
	}
	mergeValues(values, r.platformValues(profile, tenant, zone))
	placed, err := r.placementValues(context.Background(), comp, profile, zone)
	if err != nil {
		t.Fatal(err)
	}
	mergeValues(values, placed)
	return values
}

func TestTheAppStoreIsToldEveryFactItsChartReads(t *testing.T) {
	v := appStoreValues(t, "multi", "acme", true, clusterClaim("https://store.example.org/"))
	want := map[string]interface{}{
		"fullnameOverride": "app-store",
		"host":             "store.acme.k.example",
		"auth": map[string]interface{}{
			"disabled": false, "mode": "edge",
			"issuer": "https://id.k.example/auth/realms/acme", "clientId": "gentian-edge-acme", "audience": directorAudience,
		},
		"gateway":       map[string]interface{}{"enabled": false},
		"networkPolicy": map[string]interface{}{"create": false},
		"api":           map[string]interface{}{"env": map[string]interface{}{"ENVIRONMENT": "production"}},
		"director":      map[string]interface{}{"url": "http://gentian-os-director.kernel-control.svc.cluster.local:8080", "cluster": "c1"},
		"custodian":     map[string]interface{}{"url": "http://gentian-os-custodian.kernel-control.svc.cluster.local:9444"},
		"usher":         map[string]interface{}{"url": "http://gentian-os-usher.kernel-control.svc.cluster.local:8080"},
		"platform": map[string]interface{}{
			"tenant": "acme", "kernelDomain": "k.example", "realm": "acme", "zoneKind": "tenant",
		},
		// The claim's address without its trailing slash, which the chart
		// refuses.
		"store": map[string]interface{}{"url": "https://store.example.org"},
	}
	got, _ := json.MarshalIndent(v, "", "  ")
	exp, _ := json.MarshalIndent(want, "", "  ")
	if string(got) != string(exp) {
		t.Fatalf("values =\n%s\nwant\n%s", got, exp)
	}

	// The user tenant of a single-tenancy cluster is directly under the
	// cluster's domain, and store is not a name the kernel keeps there.
	single := appStoreValues(t, "single", gentianov1alpha1.SingleUserTenantName, true, clusterClaim("https://store.example.org"))
	if single["host"] != "store.k.example" {
		t.Fatalf("single-tenancy host = %v", single["host"])
	}
	if tenant := single["platform"].(map[string]interface{})["tenant"]; tenant != gentianov1alpha1.SingleUserTenantName {
		t.Fatalf("single-tenancy tenant = %v", tenant)
	}
	if reserved := reservedHostLabel("store"); reserved != "" {
		t.Fatalf("store is reserved under the cluster's domain as %s", reserved)
	}
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "single"}
	userZone := r.zoneOf(tileFixtureTenant(gentianov1alpha1.SingleUserTenantName)).zoneNames
	if userZone.domain != "k.example" {
		t.Fatalf("the user tenant's zone is %s", userZone.domain)
	}
	if refusal := reservedHostRefusal(tileFixtureComponent("app-store", "tenant-user", "app-store"),
		shippedAppStoreProfile(t), userZone, "k.example"); refusal != "" {
		t.Fatalf("the App Store app is refused its host on a single-tenancy cluster: %s", refusal)
	}

	// No store offered, no address: none named, one that is not usable, or
	// reporting off whatever the claim names.
	for name, c := range map[string]struct {
		reporting bool
		claim     *unstructured.Unstructured
	}{
		"no store named":        {true, clusterClaim("")},
		"a store over http":     {true, clusterClaim("http://store.example.org")},
		"a store with a query":  {true, clusterClaim("https://store.example.org/?x=1")},
		"reporting off":         {false, clusterClaim("https://store.example.org")},
		"reporting off, no url": {false, clusterClaim("")},
	} {
		v := appStoreValues(t, "multi", "acme", c.reporting, c.claim)
		if got := v["store"].(map[string]interface{})["url"]; got != "" {
			t.Errorf("%s: store.url = %q, want it empty", name, got)
		}
	}
}

// The values above are ones the app's chart accepts. The schema and the
// defaults are the published chart's (ghcr.io/gentian-org/charts/app-store
// 0.1.0-develop.9c4d6bb, gentian-ui apps/app-store/chart), copied to testdata:
// the schema refuses a key it does not know, a host without the store label
// and a store address with a trailing slash, and a release refused for its
// values is found on a cluster an hour into an install.
func TestTheAppStoreChartAcceptsWhatThePlatformSends(t *testing.T) {
	rawSchema, err := os.ReadFile(filepath.Join("testdata", "app-store", "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatal(err)
	}
	rawDefaults, err := os.ReadFile(filepath.Join("testdata", "app-store", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mode      string
		tenant    string
		reporting bool
		claim     *unstructured.Unstructured
	}{
		"multi, store offered":  {"multi", "acme", true, clusterClaim("https://store.example.org/")},
		"single, store offered": {"single", gentianov1alpha1.SingleUserTenantName, true, clusterClaim("https://store.example.org/api")},
		"no store":              {"multi", "acme", true, clusterClaim("")},
	} {
		sent := appStoreValues(t, c.mode, c.tenant, c.reporting, c.claim)
		// Through JSON, as the release carries them.
		raw, _ := json.Marshal(sent)
		var values map[string]interface{}
		_ = json.Unmarshal(raw, &values)
		merged := map[string]interface{}{}
		if err := yaml.Unmarshal(rawDefaults, &merged); err != nil {
			t.Fatal(err)
		}
		mergeValues(merged, values)
		if problems := checkAgainstSchema(schema, schema, merged, "values"); len(problems) > 0 {
			t.Errorf("%s: the chart's schema refuses the platform's values:\n  %s", name, strings.Join(problems, "\n  "))
		}
		// The chart itself, when one is at hand: APP_STORE_CHART names a
		// pulled chart (a directory or a .tgz).
		if chart := os.Getenv("APP_STORE_CHART"); chart != "" {
			file := filepath.Join(t.TempDir(), "values.json")
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("helm", "template", "tenant-acme-app-store", chart, "-n", "tenant-acme", "-f", file).CombinedOutput()
			if err != nil {
				t.Errorf("%s: helm template of %s with the platform's values: %v\n%s", name, chart, err, out)
			}
			for _, service := range []string{"name: app-store-api", "name: app-store-web"} {
				if !strings.Contains(string(out), service) {
					t.Errorf("%s: the chart rendered no object with %q, which the profile routes to", name, service)
				}
			}
		}
	}
	// The checker is not a rubber stamp: what the schema refuses, it refuses.
	bad := map[string]interface{}{}
	_ = yaml.Unmarshal(rawDefaults, &bad)
	mergeValues(bad, map[string]interface{}{
		"host":     "shop.acme.k.example",
		"store":    map[string]interface{}{"url": "https://store.example.org/", "token": "x"},
		"platform": map[string]interface{}{"cluster": "c1"},
		"api":      map[string]interface{}{"replicaCount": float64(2)},
	})
	if problems := checkAgainstSchema(schema, schema, bad, "values"); len(problems) != 5 {
		t.Fatalf("the schema check found %d problems in values with five: %v", len(problems), problems)
	}
}

// checkAgainstSchema checks a value against the part of JSON Schema the app's
// chart uses: object properties, required, additionalProperties false, enum,
// pattern, integer bounds, allOf and local $ref.
func checkAgainstSchema(root, schema map[string]interface{}, value interface{}, path string) []string {
	var problems []string
	if ref, ok := schema["$ref"].(string); ok {
		target := root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			target, _ = target[part].(map[string]interface{})
		}
		problems = append(problems, checkAgainstSchema(root, target, value, path)...)
	}
	if all, ok := schema["allOf"].([]interface{}); ok {
		for _, sub := range all {
			problems = append(problems, checkAgainstSchema(root, sub.(map[string]interface{}), value, path)...)
		}
	}
	if enum, ok := schema["enum"].([]interface{}); ok {
		found := false
		for _, e := range enum {
			found = found || e == value
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s: %v is not one of %v", path, value, enum))
		}
	}
	switch v := value.(type) {
	case string:
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(v) {
			problems = append(problems, fmt.Sprintf("%s: %q does not match %s", path, v, pattern))
		}
	case float64:
		if max, ok := schema["maximum"].(float64); ok && v > max {
			problems = append(problems, fmt.Sprintf("%s: %v is more than %v", path, v, max))
		}
		if min, ok := schema["minimum"].(float64); ok && v < min {
			problems = append(problems, fmt.Sprintf("%s: %v is less than %v", path, v, min))
		}
	case map[string]interface{}:
		properties, _ := schema["properties"].(map[string]interface{})
		if required, ok := schema["required"].([]interface{}); ok {
			for _, name := range required {
				if _, present := v[name.(string)]; !present {
					problems = append(problems, fmt.Sprintf("%s: %s is required", path, name))
				}
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, known := properties[k].(map[string]interface{})
			if !known {
				if extra, ok := schema["additionalProperties"].(bool); ok && !extra {
					problems = append(problems, fmt.Sprintf("%s: unknown key %s", path, k))
				}
				continue
			}
			problems = append(problems, checkAgainstSchema(root, sub, v[k], path+"."+k)...)
		}
	}
	return problems
}

// The App Store app is on a tenant exactly while the cluster offers a store
// -- it reports what it runs and its claim names one -- and never on the
// platform tenant. The tenancy mode changes its host and nothing about this.
func TestTheAppStoreIsPlacedWhereTheClusterOffersAStore(t *testing.T) {
	scheme := appStoreScheme(t)
	for _, mode := range []string{"multi", "single"} {
		for _, reporting := range []bool{true, false} {
			for _, storeURL := range []string{"https://store.example.org", ""} {
				user := acmeTenantFixture()
				if mode == "single" {
					user = tileFixtureTenant(gentianov1alpha1.SingleUserTenantName)
				}
				for _, tenant := range []*gentianov1alpha1.Tenant{user, platformTenantFixture()} {
					name := fmt.Sprintf("%s mode, reporting %v, store %q, tenant %s", mode, reporting, storeURL, tenant.Name)
					c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
						tenant.DeepCopy(), clusterClaim(storeURL), shippedAppStoreProfile(t),
						profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
					).Build()
					r := &TenantReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", KernelDomain: "k.example",
						TenancyMode: mode, LicenceReporting: reporting}
					if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					want := reporting && storeURL != "" && tenant.Name != "platform"
					err := c.Get(context.Background(), types.NamespacedName{Name: "app-store", Namespace: tenantNamespaceName(tenant)}, &gentianov1alpha1.Component{})
					if have := err == nil; have != want {
						t.Errorf("%s: App Store component present = %v, want %v (%v)", name, have, want, err)
					}
					// The components every tenant gets are not conditional.
					if err := c.Get(context.Background(), types.NamespacedName{Name: "desktop", Namespace: tenantNamespaceName(tenant)}, &gentianov1alpha1.Component{}); err != nil {
						t.Errorf("%s: the desktop is missing: %v", name, err)
					}
				}
			}
		}
	}
}

// When the cluster stops offering a store the Component this reconciler
// placed is removed, and placed again when it offers one. A Component of the
// same name that a tenant installed is the tenant's and stays; and while the
// claim cannot be read, nothing is taken away.
func TestTheAppStoreComponentFollowsTheOffer(t *testing.T) {
	scheme := appStoreScheme(t)
	ctx := context.Background()
	tenant := acmeTenantFixture()
	key := types.NamespacedName{Name: "app-store", Namespace: tenantNamespaceName(tenant)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), clusterClaim("https://store.example.org"), shippedAppStoreProfile(t)).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", LicenceReporting: true}
	present := func() bool { return c.Get(ctx, key, &gentianov1alpha1.Component{}) == nil }

	if err := r.ensureDefaultComponents(ctx, tenant); err != nil || !present() {
		t.Fatalf("not placed while a store is offered (%v)", err)
	}
	comp := &gentianov1alpha1.Component{}
	_ = c.Get(ctx, key, comp)
	if comp.Labels[componentOriginLabel] != componentOriginDefault {
		t.Fatalf("origin = %q", comp.Labels[componentOriginLabel])
	}

	// The claim stops naming a store.
	claim := clusterClaim("")
	live := clusterClaim("")
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), live); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(live.Object, "spec", "catalogue")
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureDefaultComponents(ctx, tenant); err != nil || present() {
		t.Fatalf("still placed after the claim named no store (%v)", err)
	}

	// Named again, with reporting off: still none. Reporting on: placed.
	_ = unstructured.SetNestedField(live.Object, "https://store.example.org", "spec", "catalogue", "storeUrl")
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	r.LicenceReporting = false
	if err := r.ensureDefaultComponents(ctx, tenant); err != nil || present() {
		t.Fatalf("placed with licence reporting off (%v)", err)
	}
	r.LicenceReporting = true
	if err := r.ensureDefaultComponents(ctx, tenant); err != nil || !present() {
		t.Fatalf("not placed again (%v)", err)
	}

	// The claim cannot be read: left as it is.
	failing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), shippedAppStoreProfile(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, claims := list.(*unstructured.UnstructuredList); claims {
					return fmt.Errorf("the API server is away")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	placed := comp.DeepCopy()
	placed.ResourceVersion = ""
	if err := failing.Create(ctx, placed); err != nil {
		t.Fatal(err)
	}
	rf := &TenantReconciler{Client: failing, Scheme: scheme, KernelRealm: "kernel", LicenceReporting: true}
	if err := rf.ensureDefaultComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := failing.Get(ctx, key, &gentianov1alpha1.Component{}); err != nil {
		t.Fatalf("removed while the claim could not be read: %v", err)
	}

	// A tenant's own install under that name is not this reconciler's.
	own := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), clusterClaim(""), shippedAppStoreProfile(t)).Build()
	installed := comp.DeepCopy()
	installed.ResourceVersion = ""
	installed.Labels[componentOriginLabel] = componentOriginInstall
	if err := own.Create(ctx, installed); err != nil {
		t.Fatal(err)
	}
	ro := &TenantReconciler{Client: own, Scheme: scheme, KernelRealm: "kernel", LicenceReporting: true}
	if err := ro.ensureDefaultComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := own.Get(ctx, key, &gentianov1alpha1.Component{}); err != nil {
		t.Fatalf("a tenant's own install was removed: %v", err)
	}
}

// The App Store app's pods reach the control namespace, where the director,
// the custodian and the usher are; the edge, through which the issuer of the
// forwarded token is reached; and, while a store is offered, public addresses
// on 443 -- never the cluster's own ranges, a private network or the cloud
// metadata address. With no store offered there is no way out at all.
func TestTheAppStoresWayOutIsPublicAddressesOn443(t *testing.T) {
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	t.Setenv("GENTIAN_NS_CONTROL", "kernel-control")
	scheme := appStoreScheme(t)
	ctx := context.Background()
	tenant := acmeTenantFixture()
	profile := shippedAppStoreProfile(t)
	comp := tileFixtureComponent("app-store", "tenant-acme", "app-store")
	key := types.NamespacedName{Name: "component-app-store", Namespace: "tenant-acme"}

	policy := func(reporting bool, storeURL string) *networkingv1.NetworkPolicy {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterClaim(storeURL), comp.DeepCopy()).Build()
		r := &ComponentReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", KernelDomain: "k.example", LicenceReporting: reporting}
		live := &gentianov1alpha1.Component{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(comp), live); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureNetworkPolicy(ctx, live, profile, tenant); err != nil {
			t.Fatal(err)
		}
		np := &networkingv1.NetworkPolicy{}
		if err := c.Get(ctx, key, np); err != nil {
			t.Fatal(err)
		}
		return np
	}

	np := policy(true, "https://store.example.org")
	if sel := np.Spec.PodSelector.MatchLabels[componentInstanceLabel]; sel != "tenant-acme-app-store" || len(np.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("pods selected by %v", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("egress rules = %d, want the edge, the control namespace and the way out", len(np.Spec.Egress))
	}
	var namespaces []string
	for _, rule := range np.Spec.Egress[:2] {
		namespaces = append(namespaces, rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	}
	if got := fmt.Sprint(namespaces); got != fmt.Sprint([]string{layout.Namespace(layout.Edge), layout.Namespace(layout.Control)}) {
		t.Fatalf("namespaces = %s", got)
	}
	out := np.Spec.Egress[2]
	if len(out.Ports) != 1 || out.Ports[0].Port.IntValue() != 443 || *out.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("ports = %+v", out.Ports)
	}
	if len(out.To) != 1 || out.To[0].IPBlock == nil || out.To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Fatalf("peer = %+v", out.To)
	}
	except := map[string]bool{}
	for _, cidr := range out.To[0].IPBlock.Except {
		except[cidr] = true
	}
	for _, cidr := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // private: pod, service and node ranges
		"100.64.0.0/10",  // carrier-grade NAT, which some clusters use for pods
		"169.254.0.0/16", // link-local: the cloud metadata address
		"127.0.0.0/8",
	} {
		if !except[cidr] {
			t.Errorf("%s is not excepted from the way out", cidr)
		}
	}

	// No store, no way out: only the two namespaces remain.
	for name, c := range map[string]struct {
		reporting bool
		url       string
	}{"no store named": {true, ""}, "reporting off": {false, "https://store.example.org"}} {
		np := policy(c.reporting, c.url)
		if len(np.Spec.Egress) != 2 {
			t.Errorf("%s: egress rules = %d, want the two namespaces only", name, len(np.Spec.Egress))
		}
		for _, rule := range np.Spec.Egress {
			if len(rule.To) != 1 || rule.To[0].IPBlock != nil {
				t.Errorf("%s: a rule leaves the cluster: %+v", name, rule)
			}
		}
	}

	// A profile of another tier that named the key gets no way out, whatever
	// the cluster offers. The API server refuses such a profile; this is the
	// reconciler not relying on that.
	other := profile.DeepCopy()
	other.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterClaim("https://store.example.org")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, LicenceReporting: true}
	if rules, err := r.storeEgress(ctx, other); err != nil || len(rules) != 0 {
		t.Fatalf("a certified profile got %d rules out of the cluster (%v)", len(rules), err)
	}
	placed, err := r.placementValues(ctx, comp, other, r.zoneOf(tenant))
	if err != nil || placed["store"].(map[string]interface{})["url"] != "" {
		t.Fatalf("a certified profile was told the store: %v (%v)", placed, err)
	}
}

// The verdict is written beside the tiles, for the usher: the same one that
// places the component.
func TestTheStoreOfferIsWrittenBesideTheTiles(t *testing.T) {
	scheme := appStoreScheme(t)
	for name, c := range map[string]struct {
		reporting bool
		objs      []client.Object
		want      tilecatalogue.AppStore
	}{
		"offered":        {true, []client.Object{clusterClaim("https://store.example.org")}, tilecatalogue.AppStore{Offered: true}},
		"no store named": {true, []client.Object{clusterClaim("")}, tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonNoStore}},
		"no claim":       {true, nil, tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonNoStore}},
		"reporting off":  {false, []client.Object{clusterClaim("https://store.example.org")}, tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonNoLicenceReport}},
	} {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objs...).Build()
		r := &TileProjectionReconciler{Client: cl, Cluster: "demo-cluster", KernelRealm: "kernel",
			KernelDomain: "k.example", TenancyMode: "multi", LicenceReporting: c.reporting}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cm := &corev1.ConfigMap{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: tilecatalogue.ConfigMapName, Namespace: layout.Namespace(layout.Control)}, cm); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		catalogue, err := tilecatalogue.Parse([]byte(cm.Data[tilecatalogue.Key]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if catalogue.AppStore == nil || *catalogue.AppStore != c.want {
			t.Errorf("%s: appStore = %+v, want %+v", name, catalogue.AppStore, c.want)
		}
	}
}

// A store's address is used as the app's chart accepts it, or not at all.
func TestAStoreAddressIsHTTPSWithNoQuery(t *testing.T) {
	for raw, want := range map[string]string{
		"https://store.example.org":       "https://store.example.org",
		" https://store.example.org/ ":    "https://store.example.org",
		"https://store.example.org/v1/":   "https://store.example.org/v1",
		"http://store.example.org":        "",
		"store.example.org":               "",
		"https://store.example.org/?a=b":  "",
		"https://store.example.org/#frag": "",
		"https://":                        "",
		"":                                "",
	} {
		if got := normaliseStoreAddress(raw); got != want {
			t.Errorf("normaliseStoreAddress(%q) = %q, want %q", raw, got, want)
		}
	}
}
