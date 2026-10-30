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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// mappingProfile is a chart profile that maps the given contracts' values.
func mappingProfile(m map[string]gentianov1alpha1.IntegrationValueMapping) *gentianov1alpha1.ComponentProfile {
	p := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	p.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{Integrations: m}
	return p
}

func getSensitiveValues(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) (*unstructured.Unstructured, bool) {
	t.Helper()
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	err := c.Get(context.Background(), types.NamespacedName{Name: sensitiveValuesName(comp), Namespace: comp.Namespace}, es)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return es, true
}

// markReady sets the Ready condition ESO puts on an ExternalSecret whose
// Secret it wrote.
func markReady(t *testing.T, c client.Client, es *unstructured.Unstructured) {
	t.Helper()
	if err := unstructured.SetNestedSlice(es.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "SecretSynced"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(context.Background(), es); err != nil {
		t.Fatal(err)
	}
}

// sensitiveOf answers what a render reads into the component's sensitive
// Secret -- each key and where it is read from, "<path>#<property>" -- and
// the set entries its release names, in order.
func sensitiveOf(t *testing.T, objs []*unstructured.Unstructured, app string) (data []map[string]interface{}, set []map[string]interface{}) {
	t.Helper()
	if es := renderedNamed(objs, "ExternalSecret", app+"-sensitive-values"); es != nil {
		entries, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
		for _, e := range entries {
			data = append(data, e.(map[string]interface{}))
		}
	}
	for _, release := range renderedOfKind(objs, "Release") {
		entries, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
		for _, e := range entries {
			set = append(set, e.(map[string]interface{}))
		}
	}
	return data, set
}

// The values a profile maps come out in one order -- by contract, then
// endpoint, user, password -- with each Secret key named after its contract
// and read from that contract's path in the tenant's store, and each set
// entry naming the component's own Secret. A key the profile leaves empty
// asks for nothing. The order matters: the ExternalSecret and the Release are
// compared with what is desired on every pass, and a map's order would
// rewrite both for nothing.
func TestTheMappedValuesAreReadByContractInAFixedOrder(t *testing.T) {
	profile := mappingProfile(map[string]gentianov1alpha1.IntegrationValueMapping{
		"orders":      {EndpointKey: "orders.url", PasswordKey: "orders.token"},
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL", UserKey: "api.env.DESKTOP_API_USER"},
	})
	data, set := sensitiveOf(t, renderApp(t, chartRenderInput("store", profile)), "store")

	want := []struct{ secretKey, path, property, valueKey string }{
		{"integration-desktop-api-endpoint", secrets.ContractPath("acme", "desktop-api"), "endpoint", "api.env.DESKTOP_API_URL"},
		{"integration-desktop-api-user", secrets.ContractPath("acme", "desktop-api"), "user", "api.env.DESKTOP_API_USER"},
		{"integration-orders-endpoint", secrets.ContractPath("acme", "orders"), "endpoint", "orders.url"},
		{"integration-orders-password", secrets.ContractPath("acme", "orders"), "password", "orders.token"},
	}
	if len(data) != len(want) || len(set) != len(want) {
		t.Fatalf("data = %v, set = %v, want %d entries each", data, set, len(want))
	}
	for i, w := range want {
		d := data[i]
		ref := d["remoteRef"].(map[string]interface{})
		if d["secretKey"] != w.secretKey || ref["key"] != w.path || ref["property"] != w.property {
			t.Fatalf("data[%d] = %v, want %s from %s#%s", i, d, w.secretKey, w.path, w.property)
		}
		s := set[i]
		from := s["valueFrom"].(map[string]interface{})["secretKeyRef"].(map[string]interface{})
		if s["name"] != w.valueKey || from["name"] != "store-sensitive-values" || from["namespace"] != "tenant-acme" || from["key"] != w.secretKey {
			t.Fatalf("set[%d] = %v, want %s from store-sensitive-values/%s", i, s, w.valueKey, w.secretKey)
		}
	}

	if data, set := sensitiveOf(t, renderApp(t, chartRenderInput("store", chartProfileFixture("oci://x"))), "store"); len(data) != 0 || len(set) != 0 {
		t.Fatalf("a profile that maps nothing asks for data %v, set %v", data, set)
	}
}

// mappedWikiHarness is the wiki of tenant acme with its profile mapping the
// desktop's contract endpoint.
func mappedWikiHarness(t *testing.T) *digestHarness {
	t.Helper()
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	}}
	return startDigestHarness(t, profile, "")
}

// The component's ExternalSecret is created from the tenant's own store,
// controlled by the component, and the release waits until ESO says its
// Secret is there: a set entry naming a Secret that does not exist yet fails
// the release instead of waiting. Drift is put back.
func TestTheSensitiveValuesWaitForTheirSecretAndAreKeptAsMapped(t *testing.T) {
	ctx := context.Background()
	h := mappedWikiHarness(t)

	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "ValuesPending" {
		t.Fatalf("condition = %+v, want ValuesPending", ready)
	}
	es, ok := getSensitiveValues(t, h.c, h.comp)
	if !ok {
		t.Fatal("no ExternalSecret was created")
	}
	if store, _, _ := unstructured.NestedString(es.Object, "spec", "secretStoreRef", "name"); store != secrets.TenantStore("acme") {
		t.Fatalf("store = %q, want the tenant's own", store)
	}
	if target, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name"); target != "wiki-sensitive-values" {
		t.Fatalf("target = %q", target)
	}
	if !metav1.IsControlledBy(es, got) {
		t.Fatalf("not controlled by the component: %v", es.GetOwnerReferences())
	}

	// No verdict from ESO yet.
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "ValuesPending" {
		t.Fatalf("before ESO wrote the Secret: %+v", ready)
	}
	markReady(t, h.c, es)
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason == "ValuesPending" {
		t.Fatalf("after ESO wrote the Secret: %+v", ready)
	}

	// Somebody changed where a value is read from.
	es, _ = getSensitiveValues(t, h.c, h.comp)
	if err := unstructured.SetNestedSlice(es.Object, []interface{}{map[string]interface{}{
		"secretKey": "integration-desktop-api-endpoint",
		"remoteRef": map[string]interface{}{"key": "gentian-os/tenants/other/contracts/desktop-api", "property": "endpoint"},
	}}, "spec", "data"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(ctx, es); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	es, _ = getSensitiveValues(t, h.c, h.comp)
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	key := data[0].(map[string]interface{})["remoteRef"].(map[string]interface{})["key"]
	if len(data) != 1 || key != secrets.ContractPath("acme", "desktop-api") {
		t.Fatalf("drift not corrected: data = %v", data)
	}
}

// The API server stores an ExternalSecret with the defaults of ESO's schema
// filled in (remoteRef.conversionStrategy, decodingStrategy, metadataPolicy:
// the operator chart's own ExternalSecrets spell them out for that reason).
// The component's spec is the one it wrote plus those defaults, and that is
// no drift: if it were, every pass would patch and the release wait again,
// and the component would wait for its values for ever on a real cluster.
func TestTheSensitiveValuesAreReadyDespiteTheSchemasDefaults(t *testing.T) {
	ctx := context.Background()
	h := mappedWikiHarness(t)
	h.reconcile()
	// As the API server defaults it.
	es, _ := getSensitiveValues(t, h.c, h.comp)
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, d := range data {
		ref := d.(map[string]interface{})["remoteRef"].(map[string]interface{})
		ref["conversionStrategy"], ref["decodingStrategy"], ref["metadataPolicy"] = "Default", "None", "None"
		ref["nullBytePolicy"] = "Ignore"
	}
	if err := unstructured.SetNestedSlice(es.Object, data, "spec", "data"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(ctx, es); err != nil {
		t.Fatal(err)
	}
	markReady(t, h.c, es)
	es, _ = getSensitiveValues(t, h.c, h.comp)
	before := es.GetResourceVersion()

	for pass := 1; pass <= 2; pass++ {
		if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason == "ValuesPending" {
			t.Fatalf("pass %d: %+v although ESO wrote the Secret: the schema's defaults read as drift", pass, ready)
		}
	}
	if es, _ = getSensitiveValues(t, h.c, h.comp); es.GetResourceVersion() != before {
		t.Fatal("the ExternalSecret was rewritten for the schema's defaults")
	}
}

// With nothing mapped any more, the ExternalSecret the component made goes.
// One of the same name the component did not make -- another controller's,
// or one nobody owns -- is neither taken over nor written over.
func TestTheSensitiveValuesAreRemovedWhenUnmappedAndForeignOnesLeftAlone(t *testing.T) {
	ctx := context.Background()
	h := mappedWikiHarness(t)
	h.reconcileInstalled()
	if _, ok := getSensitiveValues(t, h.c, h.comp); !ok {
		t.Fatal("no ExternalSecret was created")
	}
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Package.ValueMapping = nil })
	h.reconcile()
	if _, ok := getSensitiveValues(t, h.c, h.comp); ok {
		t.Fatal("the ExternalSecret stayed after nothing was mapped any more")
	}

	controller := true
	for name, owners := range map[string][]metav1.OwnerReference{
		"another controller's": {{APIVersion: "gentianos.io/v1alpha1", Kind: "XApp", Name: "wiki-xapp", UID: "uid-xapp", Controller: &controller}},
		"nobody's":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := mappedWikiHarness(t)
			foreign := &unstructured.Unstructured{}
			foreign.SetGroupVersionKind(externalSecretGVK)
			foreign.SetName(sensitiveValuesName(h.comp))
			foreign.SetNamespace(h.comp.Namespace)
			foreign.SetOwnerReferences(owners)
			_ = unstructured.SetNestedField(foreign.Object, "composed", "spec", "refreshInterval")
			if err := h.c.Create(ctx, foreign); err != nil {
				t.Fatal(err)
			}
			if _, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)}); err == nil {
				t.Fatal("an ExternalSecret this component did not make was taken over")
			}
			es, _ := getSensitiveValues(t, h.c, h.comp)
			if got, _, _ := unstructured.NestedString(es.Object, "spec", "refreshInterval"); got != "composed" || len(es.GetOwnerReferences()) != len(owners) {
				t.Fatalf("an ExternalSecret this component did not make was written over: %v", es.Object)
			}
		})
	}
}

// The Release names each mapped value with a set entry reading the
// component's Secret.
func TestTheReleaseIsToldTheMappedValuesBySetEntries(t *testing.T) {
	ctx := context.Background()
	h := mappedWikiHarness(t)
	releaseSet := func() ([]interface{}, bool) {
		t.Helper()
		release := &unstructured.Unstructured{}
		release.SetGroupVersionKind(helmReleaseGVK)
		if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
			t.Fatal(err)
		}
		set, found, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
		return set, found
	}

	h.reconcileInstalled()
	set, _ := releaseSet()
	if len(set) != 1 {
		t.Fatalf("set = %v, want the one mapped value", set)
	}
	entry := set[0].(map[string]interface{})
	ref, _, _ := unstructured.NestedStringMap(entry, "valueFrom", "secretKeyRef")
	if entry["name"] != "api.env.DESKTOP_API_URL" || ref["name"] != "wiki-sensitive-values" ||
		ref["namespace"] != "tenant-acme" || ref["key"] != "integration-desktop-api-endpoint" {
		t.Fatalf("set entry = %v", entry)
	}

}

// What the render no longer says is taken off the live object, not only what
// it says differently: a set entry no longer mapped, and a value the
// profile's extraValues no longer carry. A set entry left behind reads a key
// of a Secret that is removed in the same pass, and the release fails.
func TestWhatTheRenderNoLongerSaysIsTakenOffTheRelease(t *testing.T) {
	ctx := context.Background()
	h := mappedWikiHarness(t)
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Package.ExtraValues = &runtime.RawExtension{Raw: []byte(`{"feature":{"old":true}}`)}
	})
	h.reconcileInstalled()
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Package.ValueMapping = nil
		p.Spec.Package.ExtraValues = nil
	})
	h.reconcile()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatal(err)
	}
	if set, found, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set"); found && len(set) > 0 {
		t.Errorf("set = %v after nothing is mapped any more", set)
	}
	if old, found, _ := unstructured.NestedFieldNoCopy(release.Object, "spec", "forProvider", "values", "feature"); found {
		t.Errorf("values.feature = %v after the profile stopped carrying it", old)
	}
}

// A component that maps a contract's values waits for its Secret with
// ValuesPending and has no Release meanwhile; once ESO has written it, the
// Release is made with the set entry that reads it.
func TestAComponentWaitsForItsMappedValuesBeforeItsRelease(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	}}
	h := startDigestHarness(t, profile, "")

	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "ValuesPending" {
		t.Fatalf("condition = %+v, want ValuesPending", ready)
	}
	if h.releasedVersion() != "" {
		t.Fatal("a release was made before the Secret it reads was there")
	}
	es, ok := getSensitiveValues(t, h.c, h.comp)
	if !ok {
		t.Fatal("no ExternalSecret for the mapped values")
	}
	markReady(t, h.c, es)

	got = h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason == "ValuesPending" {
		t.Fatalf("condition = %+v, still waiting after the Secret arrived", ready)
	}
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatalf("no release after the Secret arrived: %v", err)
	}
	set, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
	if len(set) != 1 || set[0].(map[string]interface{})["name"] != "api.env.DESKTOP_API_URL" {
		t.Fatalf("set = %v", set)
	}
}

// The issue's acceptance: a contract between a placed and an installed
// component opens its network path and delivers the endpoint value.
//
// The desktop is placed (placement every, platform trust) and provides
// desktop-api at desktop-gentian-portal-api:8000; the store is installed and
// integrates with it, mapping the endpoint to api.env.DESKTOP_API_URL; the
// tenant grants the store consume desktop-api [tiles]. Each step is the
// reconciler that does it on a cluster, run in turn on one fake client: the
// grant becomes an AppGrant, the binding carries the provider's endpoint, the
// contract's two policies select the desktop by its release's instance label
// (it is placed) and the store by its app label on port 8000, the binding's
// reconciler writes the address at the contract's path, and the store's
// render -- applied as the component reconciler applies it -- reads it from
// that same path into its ExternalSecret and tells its release. Withdrawn,
// the grant takes the policies and the address with it.
func TestAContractBetweenAPlacedAndAnInstalledComponentOpensItsPathAndDeliversTheEndpoint(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)

	desktop := chartProfileFixture("oci://registry.example/gentian")
	desktop.Name = "desktop"
	desktop.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	desktop.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	desktop.Spec.Provides = []gentianov1alpha1.ContractRef{{
		Name: "desktop-api", Protocol: "http",
		Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "desktop-gentian-portal-api", Port: 8000},
	}}
	store := mappingProfile(map[string]gentianov1alpha1.IntegrationValueMapping{
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	})
	store.Name = "store"
	store.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "desktop-api", Provider: "desktop", Capabilities: []string{"tiles"}}}
	if !operatorPlaced(desktop) || operatorPlaced(store) {
		t.Fatal("the fixture is meant to be a placed desktop and an installed store")
	}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop"), {Profile: "store"}}
	tenant.Spec.Grants = []gentianov1alpha1.TenantAppGrant{{AppGrantSpec: gentianov1alpha1.AppGrantSpec{
		App: "store", Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "desktop-api", Granted: []string{"tiles"}}},
	}}}
	ns := tenantNamespaceName(tenant)
	path := secrets.ContractPath(tenant.Name, "desktop-api")
	vault := memVault{path: {"password": "shared", "endpoint": "", "user": ""}}
	seeder := secrets.NewSeeder(vault, nil)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), desktop, store).
		WithStatusSubresource(&gentianov1alpha1.IntegrationBinding{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme, Seeder: seeder}

	// The grant, then the binding the tenant's data plane applies.
	if err := tr.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	bindings, err := tr.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want the store's one", len(bindings))
	}
	binding := bindings[0]
	if binding.Spec.Provider.App != "desktop" || binding.Spec.Consumer.App != "store" {
		t.Fatalf("binding = %+v", binding.Spec)
	}
	if e := binding.Spec.Endpoint; e == nil || e.Service != "desktop-gentian-portal-api" || e.Port != 8000 {
		t.Fatalf("binding endpoint = %+v, want the provider's", binding.Spec.Endpoint)
	}
	if err := c.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}

	// The network path.
	if err := tr.ensureNetworkPolicies(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	desktopPods := map[string]string{componentInstanceLabel: ns + "-desktop"}
	storePods := map[string]string{appLabel: "store"}
	out := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "contract-" + binding.Name, Namespace: ns}, out); err != nil {
		t.Fatalf("no way out for the consumer: %v", err)
	}
	if !equalLabels(out.Spec.PodSelector.MatchLabels, storePods) || len(out.Spec.Egress) != 1 ||
		len(out.Spec.Egress[0].To) != 1 || !equalLabels(out.Spec.Egress[0].To[0].PodSelector.MatchLabels, desktopPods) ||
		!onlyPort(out.Spec.Egress[0].Ports, 8000) {
		t.Fatalf("consumer policy = %+v", out.Spec)
	}
	in := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "contract-in-" + binding.Name, Namespace: ns}, in); err != nil {
		t.Fatalf("no way in at the provider: %v", err)
	}
	if !equalLabels(in.Spec.PodSelector.MatchLabels, desktopPods) || len(in.Spec.Ingress) != 1 ||
		len(in.Spec.Ingress[0].From) != 1 || !equalLabels(in.Spec.Ingress[0].From[0].PodSelector.MatchLabels, storePods) ||
		!onlyPort(in.Spec.Ingress[0].Ports, 8000) {
		t.Fatalf("provider policy = %+v", in.Spec)
	}

	// The address, at the path the binding names.
	br := &IntegrationBindingReconciler{Client: c, Scheme: scheme, Seeder: seeder}
	if _, err := br.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(binding)}); err != nil {
		t.Fatal(err)
	}
	const address = "http://desktop-gentian-portal-api.tenant-acme.svc.cluster.local:8000"
	if got := vault[path]["endpoint"]; got != address {
		t.Fatalf("endpoint at %s = %q, want %q", path, got, address)
	}
	if binding.Spec.Auth == nil || binding.Spec.Auth.VaultPath != path {
		t.Fatalf("the binding names %+v, not the path the consumer reads", binding.Spec.Auth)
	}

	// The consumer's chart reads it from there.
	consumer := componentFor("store", ns)
	consumer.UID = "uid-store"
	if err := c.Create(ctx, consumer); err != nil {
		t.Fatal(err)
	}
	cr := &ComponentReconciler{Client: c, Scheme: scheme}
	rendered, err := cr.render(ctx, mustRenderInput(t, cr, consumer, store, tenant))
	if err != nil {
		t.Fatal(err)
	}
	if _, reason, _, err := cr.applyRendered(ctx, consumer, rendered); err != nil || reason != "ValuesPending" {
		t.Fatalf("reason %q, err %v; want the release to wait for the endpoint's Secret", reason, err)
	}
	es, ok := getSensitiveValues(t, c, consumer)
	if !ok {
		t.Fatal("the consumer has no ExternalSecret for the endpoint")
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	if len(data) != 1 {
		t.Fatalf("data = %v", data)
	}
	d := data[0].(map[string]interface{})
	ref := d["remoteRef"].(map[string]interface{})
	if d["secretKey"] != "integration-desktop-api-endpoint" || ref["key"] != path || ref["property"] != "endpoint" {
		t.Fatalf("the consumer reads %v, not the endpoint at %s", d, path)
	}
	markReady(t, c, es)
	if _, reason, _, err := cr.applyRendered(ctx, consumer, rendered); err != nil || reason == "ValuesPending" {
		t.Fatalf("reason %q, err %v; want the release written once the Secret is there", reason, err)
	}
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: releaseName(consumer)}, release); err != nil {
		t.Fatalf("no release for the consumer: %v", err)
	}
	if set, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set"); len(set) != 1 || set[0].(map[string]interface{})["name"] != "api.env.DESKTOP_API_URL" {
		t.Fatalf("set = %v", set)
	}

	// Withdrawn: no way between the two, and no address.
	tenant.Spec.Grants = nil
	if err := tr.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := tr.ensureNetworkPolicies(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"contract-" + binding.Name, "contract-in-" + binding.Name} {
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &networkingv1.NetworkPolicy{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("%s outlived the grant: %v", name, err)
		}
	}
	mapped, err := tr.mappedContracts(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.clearUngrantedContractEndpoints(ctx, tenant, bindings, mapped); err != nil {
		t.Fatal(err)
	}
	if got := vault[path]["endpoint"]; got != "" {
		t.Fatalf("endpoint after the grant was withdrawn = %q, want empty", got)
	}
	if got := vault[path]["password"]; got != "shared" {
		t.Fatalf("the shared password became %q", got)
	}
}

func equalLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func onlyPort(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	return len(ports) == 1 && ports[0].Port != nil && ports[0].Port.IntVal == port
}

// A Ready condition counts only for the generation it was synced from: one
// left from before the spec changed says nothing about the keys asked for
// now, and a release told of a key ESO has not written yet would fail.
func TestASensitiveSecretIsSyncedOnlyFromItsCurrentGeneration(t *testing.T) {
	es := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"generation": int64(2)},
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}},
		},
	}}
	if !externalSecretSynced(es) {
		t.Error("Ready with no synced version recorded: want synced")
	}
	_ = unstructured.SetNestedField(es.Object, "1-abc", "status", "syncedResourceVersion")
	if externalSecretSynced(es) {
		t.Error("Ready from generation 1 while the spec is at 2: want not synced")
	}
	_ = unstructured.SetNestedField(es.Object, "2-def", "status", "syncedResourceVersion")
	if !externalSecretSynced(es) {
		t.Error("Ready from the current generation: want synced")
	}
}

// mustRenderInput gathers what a component is rendered from, as Reconcile
// does for one with no pull secrets, sign-in sidecar or operator values.
func mustRenderInput(t *testing.T, r *ComponentReconciler, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) *apprender.Input {
	t.Helper()
	in, err := r.renderInput(context.Background(), comp, profile, tenant, r.zoneOf(tenant), pullSecrets{}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.Placed = operatorPlaced(profile)
	in.Consumer = gentianov1alpha1.CredentialConsumer{Kind: "Component", Name: comp.Name}
	return in
}

// Object storage, the cache and mail reach the chart as the app Composition
// gave them: each read from where the tenant's stores wrote it for the app,
// and mail -- optional, empty on a cluster without the relay's credentials --
// as a values document of its own that the release reads only when it is
// there.
func TestStorageTheCacheAndMailAreToldAsTheCompositionTellsThem(t *testing.T) {
	profile := chartProfileFixture("oci://x")
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		S3:    &gentianov1alpha1.S3ValueMapping{EndpointKey: "s3.endpoint", BucketKey: "s3.bucket"},
		Cache: &gentianov1alpha1.CacheValueMapping{HostKey: "cache.host", PasswordKey: "cache.password"},
		SMTP:  &gentianov1alpha1.SMTPValueMapping{HostKey: "mail.smtp.host", PortKey: "mail.smtp.port", PasswordKey: "mail.smtp.password"},
		IMAP:  &gentianov1alpha1.IMAPValueMapping{HostKey: "mail.imap.host"},
	}
	in := chartRenderInput("app", profile)
	in.Namespace = "tenant-demo"
	objs := renderApp(t, in)
	data, setEntries := sensitiveOf(t, objs, "app")
	read := map[string]string{}
	for _, e := range data {
		ref := e["remoteRef"].(map[string]interface{})
		read[e["secretKey"].(string)] = ref["key"].(string) + "#" + ref["property"].(string)
	}
	apps := "gentian-os/tenants/demo/apps/app/"
	for key, want := range map[string]string{
		"s3-endpoint": apps + "s3#endpoint", "s3-bucket": apps + "s3#bucket",
		"cache-host": apps + "cache#host", "cache-password": apps + "cache#password",
		"smtp-host": apps + "smtp#host", "smtp-port": apps + "smtp#port", "smtp-password": apps + "smtp#password",
		"imap-host": apps + "imap#host",
	} {
		if read[key] != want {
			t.Errorf("%s reads %q, want %q", key, read[key], want)
		}
	}
	var set []string
	for _, e := range setEntries {
		set = append(set, e["name"].(string))
	}
	if want := []string{"s3.endpoint", "s3.bucket", "cache.host", "cache.password", "mail.imap.host"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v: mail is no set entry", set, want)
	}
	es := renderedNamed(objs, "ExternalSecret", "app-sensitive-values")
	documents, _, _ := unstructured.NestedStringMap(es.Object, "spec", "target", "template", "data")
	for doc, want := range map[string]string{
		"values-smtp-host":     `{{ with index . "smtp-host" }}{"mail":{"smtp":{"host":{{ . | toJson }}}}}{{ end }}`,
		"values-smtp-password": `{{ with index . "smtp-password" }}{"mail":{"smtp":{"password":{{ . | toJson }}}}}{{ end }}`,
		"values-smtp-port":     `{{ with index . "smtp-port" }}{"mail":{"smtp":{"port":{{ . }}}}}{{ end }}`,
	} {
		if documents[doc] != want {
			t.Errorf("%s = %q, want %q", doc, documents[doc], want)
		}
	}
	var valuesFrom []interface{}
	for _, release := range renderedOfKind(objs, "Release") {
		valuesFrom, _, _ = unstructured.NestedSlice(release.Object, "spec", "forProvider", "valuesFrom")
	}
	if len(valuesFrom) != 3 {
		t.Fatalf("valuesFrom = %v, want one optional read per mail value", valuesFrom)
	}
	for _, f := range valuesFrom {
		ref := f.(map[string]interface{})["secretKeyRef"].(map[string]interface{})
		if ref["name"] != "app-sensitive-values" || ref["optional"] != true {
			t.Errorf("valuesFrom %v, want an optional read of the sensitive Secret", ref)
		}
	}
}
