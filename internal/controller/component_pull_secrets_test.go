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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func repositoryClaimFixture(namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(repositoryClaimGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedField(u.Object, spec, "spec")
	return u
}

func ociRepositorySpec(tenant, address string) map[string]interface{} {
	spec := map[string]interface{}{
		"type":       "oci",
		"role":       "apps",
		"endpoints":  map[string]interface{}{"inCluster": address},
		"credential": map[string]interface{}{"vaultPath": "gentian-os/tenants/" + tenant + "/repositories/x", "displayName": "x"},
	}
	if tenant != "" {
		spec["tenant"] = tenant
	}
	return spec
}

func chartProfileFixture(repository string, extensions ...string) *gentianov1alpha1.ComponentProfile {
	p := profileFixture("vendor-app", false, gentianov1alpha1.ComponentClassApp)
	p.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Repository: repository, Name: "vendor-app", Version: "1.0.0"}
	for _, e := range extensions {
		p.Spec.Extensions = append(p.Spec.Extensions, gentianov1alpha1.AppSidecarSpec{
			Name: "ext", Chart: gentianov1alpha1.ChartRef{Repository: e, Name: "ext", Version: "1.0.0"},
		})
	}
	return p
}

// A chart lies inside a repository when the host is the same and the
// repository's path is a prefix of the chart's by whole segments. The scheme
// says nothing about where a registry is.
func TestAChartIsMatchedToARepositoryByHostAndPathPrefix(t *testing.T) {
	repo := func(name, address string) pullRepository {
		host, path := registryAddress(address)
		return pullRepository{name: name, host: host, path: path}
	}
	for _, tc := range []struct {
		name, chart, declared string
		want                  bool
	}{
		{"same host, no path", "oci://registry.vendor.example/acme/charts", "https://registry.vendor.example", true},
		{"scheme differs", "oci://registry.vendor.example/acme", "oci://registry.vendor.example/acme/", true},
		{"path prefix", "oci://registry.vendor.example/acme/charts", "registry.vendor.example/acme", true},
		{"host case", "oci://Registry.Vendor.Example/acme", "https://registry.vendor.example/acme", true},
		{"a prefix of a segment is not a prefix", "oci://registry.vendor.example/acme-other/charts", "https://registry.vendor.example/acme", false},
		{"repository deeper than the chart", "oci://registry.vendor.example/acme", "https://registry.vendor.example/acme/charts", false},
		{"another host", "oci://registry.other.example/acme", "https://registry.vendor.example", false},
		{"a host that only ends the same", "oci://evilregistry.vendor.example/acme", "https://registry.vendor.example", false},
		{"another port", "oci://registry.vendor.example:5000/acme", "https://registry.vendor.example", false},
		{"the same port", "oci://registry.vendor.example:5000/acme", "https://registry.vendor.example:5000", true},
		{"the host named in a chart's path", "oci://registry.other.example/registry.vendor.example/acme", "https://registry.vendor.example", false},
		{"userinfo is not the host", "oci://registry.vendor.example@registry.other.example/acme", "https://registry.vendor.example", false},
		{"an empty declaration matches nothing", "oci://registry.vendor.example/acme", "", false},
	} {
		got := len(matchChartRepository(tc.chart, []pullRepository{repo("vendor", tc.declared)})) == 1
		if got != tc.want {
			t.Errorf("%s: chart %q in repository %q = %v, want %v", tc.name, tc.chart, tc.declared, got, tc.want)
		}
	}
}

// Only the tenant's own registry repositories count, by an exact match on
// the tenant: not another tenant's, not the cluster's, not one whose name
// merely starts the same, and not a claim outside the namespace the director
// declares them in.
func TestOnlyTheTenantsOwnRepositoriesAreRead(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)

	git := ociRepositorySpec("acme", "https://git.vendor.example/acme")
	git["type"] = "git"
	public := ociRepositorySpec("acme", "https://public.vendor.example")
	delete(public, "credential")
	bearer := ociRepositorySpec("acme", "https://token.vendor.example")
	bearer["credential"].(map[string]interface{})["authType"] = "bearer"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		repositoryClaimFixture(repositoryClaimNamespace, "vendor-apps", ociRepositorySpec("acme", "oci://registry.vendor.example/acme")),
		repositoryClaimFixture(repositoryClaimNamespace, "globex-apps", ociRepositorySpec("globex", "oci://registry.vendor.example/globex")),
		repositoryClaimFixture(repositoryClaimNamespace, "acme-two-apps", ociRepositorySpec("acme-two", "oci://registry.vendor.example/acme-two")),
		repositoryClaimFixture(repositoryClaimNamespace, "cluster-charts", ociRepositorySpec("", "https://registry.cluster.example")),
		repositoryClaimFixture(repositoryClaimNamespace, "acme-git", git),
		repositoryClaimFixture(repositoryClaimNamespace, "acme-public", public),
		repositoryClaimFixture(repositoryClaimNamespace, "acme-bearer", bearer),
		// Says it is acme's, from a namespace nothing is declared in.
		repositoryClaimFixture("tenant-globex", "planted", ociRepositorySpec("acme", "oci://registry.planted.example")),
	).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	names := func(tenant string) string {
		repos, err := r.tenantPullRepositories(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, repo := range repos {
			out = append(out, repo.secretName())
		}
		return strings.Join(out, " ")
	}
	if got := names("acme"); got != "repository-vendor-apps-pull" {
		t.Fatalf("acme pulls with %q", got)
	}
	if got := names("globex"); got != "repository-globex-apps-pull" {
		t.Fatalf("globex pulls with %q", got)
	}
	if got := names("initech"); got != "" {
		t.Fatalf("a tenant that declared nothing pulls with %q", got)
	}
	// The cluster's repositories have no tenant, and asking with none must
	// not return them.
	if got := names(""); got != "" {
		t.Fatalf("no tenant pulls with %q", got)
	}
}

// One match sets the chart's Secret; none leaves it a public chart; more
// than one is refused rather than guessed. Every declared repository is
// offered to the pods either way.
func TestPullSecretsAreResolvedForAProfile(t *testing.T) {
	repo := func(name, address string) pullRepository {
		host, path := registryAddress(address)
		return pullRepository{name: name, host: host, path: path}
	}
	vendor := repo("vendor-apps", "oci://registry.vendor.example/acme")
	other := repo("other", "https://registry.other.example")

	got, refusal := resolvePullSecrets(
		chartProfileFixture("oci://registry.vendor.example/acme/charts", "oci://public.example/ext"),
		[]pullRepository{other, vendor})
	if refusal != "" {
		t.Fatal(refusal)
	}
	if !reflect.DeepEqual(got.images, []string{"repository-other-pull", "repository-vendor-apps-pull"}) {
		t.Fatalf("images = %v", got.images)
	}
	if c := got.chart("oci://registry.vendor.example/acme/charts"); c == nil || c.secretName != "repository-vendor-apps-pull" {
		t.Fatalf("chart = %+v", c)
	}
	if c := got.chart("oci://public.example/ext"); c != nil {
		t.Fatalf("a chart in no declared repository is pulled with %+v", c)
	}

	// A public chart: nothing is named on the release.
	got, refusal = resolvePullSecrets(chartProfileFixture("oci://public.example/app"), []pullRepository{vendor})
	if refusal != "" || len(got.charts) != 0 {
		t.Fatalf("charts = %+v refusal = %q", got.charts, refusal)
	}

	// No declared repository: nothing at all.
	got, refusal = resolvePullSecrets(chartProfileFixture("oci://registry.vendor.example/acme/charts"), nil)
	if refusal != "" || len(got.charts) != 0 || len(got.images) != 0 {
		t.Fatalf("got = %+v refusal = %q", got, refusal)
	}

	// Two repositories the chart lies inside.
	wide := repo("vendor-all", "https://registry.vendor.example")
	got, refusal = resolvePullSecrets(chartProfileFixture("oci://registry.vendor.example/acme/charts"), []pullRepository{vendor, wide})
	if !strings.Contains(refusal, "vendor-apps") || !strings.Contains(refusal, "vendor-all") {
		t.Fatalf("refusal = %q", refusal)
	}
	if len(got.charts) != 0 || len(got.images) != 0 {
		t.Fatalf("an ambiguous match still resolved to %+v", got)
	}
}

// The tenant's Secrets follow the profile's own, or the cluster's when the
// profile names none, in the shape the profile says its chart takes. A
// tenant with no registry repository leaves the values untouched.
func TestPullSecretsAreNamedInTheChartsValues(t *testing.T) {
	profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")

	untouched := map[string]interface{}{"replicas": int64(1)}
	valuesWithPullSecrets(untouched, profile, nil)
	if !reflect.DeepEqual(untouched, map[string]interface{}{"replicas": int64(1)}) {
		t.Fatalf("values changed with nothing declared: %v", untouched)
	}

	values := map[string]interface{}{"global": map[string]interface{}{"storageClass": "fast"}}
	valuesWithPullSecrets(values, profile, []string{"repository-vendor-apps-pull"})
	want := []interface{}{"registry-credentials", "repository-vendor-apps-pull"}
	if !reflect.DeepEqual(values["imagePullSecrets"], want) {
		t.Fatalf("imagePullSecrets = %v", values["imagePullSecrets"])
	}
	global := values["global"].(map[string]interface{})
	if !reflect.DeepEqual(global["imagePullSecrets"], want) || global["storageClass"] != "fast" {
		t.Fatalf("global = %v", global)
	}

	profile.Annotations = map[string]string{standardPullSecretsAnnotation: "true"}
	values = map[string]interface{}{"imagePullSecrets": []interface{}{map[string]interface{}{"name": "own"}}}
	valuesWithPullSecrets(values, profile, []string{"repository-vendor-apps-pull"})
	wantMaps := []interface{}{
		map[string]interface{}{"name": "own"},
		map[string]interface{}{"name": "repository-vendor-apps-pull"},
	}
	if !reflect.DeepEqual(values["imagePullSecrets"], wantMaps) {
		t.Fatalf("imagePullSecrets = %v", values["imagePullSecrets"])
	}
}

// A chart delivered directly is pulled with the Secret in the component's
// own namespace; one in no declared repository names none.
func TestTheReleaseNamesTheChartsPullSecretInItsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	pull := pullSecrets{charts: []chartPull{{
		repository: "oci://registry.vendor.example/acme/charts", secretName: "repository-vendor-apps-pull", declared: "vendor-apps",
	}}}
	chartOf := func(tenantNS string, pull pullSecrets) map[string]interface{} {
		comp := &gentianov1alpha1.Component{}
		comp.Name, comp.Namespace = "vendor-app", tenantNS
		if _, _, err := r.ensureRelease(ctx, comp, profile, map[string]interface{}{}, pull); err != nil {
			t.Fatal(err)
		}
		release := &unstructured.Unstructured{}
		release.SetGroupVersionKind(helmReleaseGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: releaseName(comp)}, release); err != nil {
			t.Fatal(err)
		}
		chart, _, _ := unstructured.NestedMap(release.Object, "spec", "forProvider", "chart")
		return chart
	}

	acme := chartOf("tenant-acme", pull)
	ref, _ := acme["pullSecretRef"].(map[string]interface{})
	if ref["name"] != "repository-vendor-apps-pull" || ref["namespace"] != "tenant-acme" {
		t.Fatalf("acme's chart is pulled with %v", acme["pullSecretRef"])
	}
	// The same profile in a tenant that declared no repository.
	if globex := chartOf("tenant-globex", pullSecrets{}); globex["pullSecretRef"] != nil {
		t.Fatalf("globex's chart is pulled with %v", globex["pullSecretRef"])
	}
}

// The App claim carries the names to the Composition, and stops carrying
// them when the tenant no longer declares the repository.
func TestTheAppClaimCarriesThePullSecrets(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "vendor-app", tenantNamespaceName(tenant), "uid-comp"
	comp.Spec.ProfileRef.Name = "vendor-app"
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	zone := edgeZone{domain: "acme.k.example"}

	pull := pullSecrets{
		images: []string{"repository-vendor-apps-pull"},
		charts: []chartPull{{repository: "oci://registry.vendor.example/acme/charts", secretName: "repository-vendor-apps-pull"}},
	}
	claimed := func() (map[string]interface{}, bool) {
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
			t.Fatal(err)
		}
		got, has, _ := unstructured.NestedMap(claim.Object, "spec", "pullSecrets")
		return got, has
	}

	if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pull); err != nil {
		t.Fatal(err)
	}
	got, _ := claimed()
	want := map[string]interface{}{
		"images": []interface{}{"repository-vendor-apps-pull"},
		"charts": []interface{}{map[string]interface{}{
			"repository": "oci://registry.vendor.example/acme/charts", "secretName": "repository-vendor-apps-pull",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pullSecrets = %v", got)
	}

	if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}); err != nil {
		t.Fatal(err)
	}
	if got, has := claimed(); has {
		t.Fatalf("the claim still names %v", got)
	}
}

// A release that cannot pull its chart says so on the Component, with the
// repository whose credential it was pulled with.
func TestAFailedChartPullNamesTheRepository(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)

	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	release.SetName("vendor-app-x1-release")
	_ = unstructured.SetNestedSlice(release.Object, []interface{}{map[string]interface{}{
		"type": "Synced", "status": "False", "message": "failed to pull chart: 401 Unauthorized",
	}}, "status", "conditions")
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	_ = unstructured.SetNestedField(claim.Object, "vendor-app-x1", "spec", "resourceRef", "name")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(release).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	message := r.composedReleaseMessage(ctx, claim)
	if !strings.Contains(message, "401 Unauthorized") {
		t.Fatalf("message = %q", message)
	}

	profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	pull := pullSecrets{charts: []chartPull{{
		repository: "oci://registry.vendor.example/acme/charts", secretName: "repository-vendor-apps-pull", declared: "vendor-apps",
	}}}
	if hinted := pull.withPullHint(message, profile); !strings.Contains(hinted, "repository vendor-apps") || !strings.Contains(hinted, "401") {
		t.Fatalf("hinted = %q", hinted)
	}
	// A chart pulled with no credential of the tenant's gets no such hint.
	if plain := (pullSecrets{}).withPullHint(message, profile); plain != message {
		t.Fatalf("plain = %q", plain)
	}
}
