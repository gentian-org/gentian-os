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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const envoyGatewayCRDs = "testdata/envoy-gateway-crds"

// The definitions the test below validates against are the pinned release's.
// A new pin without new definitions would leave it validating against the old
// ones and passing.
func TestTheEnvoyGatewayDefinitionsAreThePinnedReleases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "versions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pins map[string]map[string]string
	if err := yaml.Unmarshal(raw, &pins); err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(filepath.Join(envoyGatewayCRDs, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(have)), pins["envoy-gateway"]["chart"]; got != want || want == "" {
		t.Fatalf("%s holds the CRDs of %q and versions.yaml pins %q: run scripts/gen/gen-envoy-gateway-crds.py", envoyGatewayCRDs, got, want)
	}
}

// edgeEnvoyProxy renders the EnvoyProxy the installer applies.
func edgeEnvoyProxy(t *testing.T, serviceType string, extra ...string) *unstructured.Unstructured {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is needed to render the edge EnvoyProxy as the installer does: %v", err)
	}
	root := filepath.Join("..", "..")
	args := []string{
		"template", "gentian-edge", filepath.Join(root, "kernel", "manifests", "gateway", "chart"),
		"-f", filepath.Join(root, "kernel", "platforms.yaml"),
		"--set", "namespace=" + servicesNamespace,
		"--set-string", "envoy.serviceType=" + serviceType,
	}
	args = append(args, extra...)
	var stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(out, &obj.Object); err != nil {
		t.Fatal(err)
	}
	if obj.GetKind() != "EnvoyProxy" {
		t.Fatalf("the chart rendered a %s", obj.GetKind())
	}
	return obj
}

// The order at the front door, as it is configured: the session filter moved
// ahead of the bouncer, in both shapes of the data plane. Envoy Gateway's own
// order has them the other way round, and with that order a request reaches
// the bouncer before anything has established or refreshed its session.
func TestTheEdgeRunsTheSessionFilterBeforeTheBouncer(t *testing.T) {
	for _, proxy := range []*unstructured.Unstructured{
		edgeEnvoyProxy(t, "ClusterIP"),
		edgeEnvoyProxy(t, "LoadBalancer", "--set", "nodeIp=203.0.113.10"),
	} {
		order, _, err := unstructured.NestedSlice(proxy.Object, "spec", "filterOrder")
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != 1 {
			t.Fatalf("filterOrder = %v, want the one move", order)
		}
		move := order[0].(map[string]interface{})
		if move["name"] != "envoy.filters.http.oauth2" || move["before"] != "envoy.filters.http.ext_authz" {
			t.Fatalf("filterOrder = %v: the OAuth2 filter must be moved before ext_authz", move)
		}
		if _, both := move["after"]; both {
			t.Fatalf("filterOrder = %v", move)
		}
		// Both Gateways are one fleet, so one EnvoyProxy orders both.
		if merged, _, _ := unstructured.NestedBool(proxy.Object, "spec", "mergeGateways"); !merged {
			t.Fatal("mergeGateways is off: the perimeter Gateway, which carries a session route, would be another fleet")
		}
	}
}

// Everything this repository writes for Envoy Gateway, created against the
// pinned release's own definitions with unknown fields refused.
//
// The operator builds these objects as untyped maps and the EnvoyProxy comes
// out of a chart, so nothing else stands between a misspelt or removed field
// and an install that finds it: the API server drops what it does not know,
// and a policy that silently lost a field is a policy that does something
// else.
func TestWhatIsWrittenForEnvoyGatewayIsValidForThePinnedRelease(t *testing.T) {
	binDir := "/tmp/envtest-bins/k8s/1.32.0-linux-amd64"
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		binDir = v
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{envoyGatewayCRDs},
		BinaryAssetsDirectory: binDir,
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("the pinned release's definitions do not install: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Namespace"))
	ns.SetName(servicesNamespace)
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	seq := 0
	create := func(what string, gvk schema.GroupVersionKind, spec map[string]interface{}) {
		t.Helper()
		seq++
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetName(fmt.Sprintf("check-%d", seq))
		obj.SetNamespace(servicesNamespace)
		if err := unstructured.SetNestedField(obj.Object, runtime.DeepCopyJSON(jsonSafe(t, spec)), "spec"); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, obj, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Errorf("%s is not a valid %s for the pinned release: %v", what, gvk.Kind, err)
		}
	}

	// The kernel's routes, in both tenancy modes, with everything switched on.
	policies := 0
	for _, door := range []kernelFrontDoor{{}, {single: true, userDesktop: "desktop.k.example"}} {
		specs := kernelHTTPRouteSpecs("k.example", []string{"acme.example"}, nil, []string{"acme"}, true, "c1", true, true, door)
		for _, s := range specs {
			switch {
			case s.authz != nil:
				create("the session policy of "+s.name, securityPolicyGVK,
					kernelSecurityPolicySpec("k.example", "kernel", s.name, *s.authz, "gentian-os-bouncer"))
				policies++
			case s.securityPolicy != nil:
				spec := cloneMap(s.securityPolicy)
				spec["targetRefs"] = []interface{}{
					map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": s.name},
				}
				create("the policy of "+s.name, securityPolicyGVK, spec)
				policies++
			}
			if s.policy != nil {
				spec := cloneMap(s.policy)
				attachBackendTrafficPolicyTarget(spec, s.name)
				create("the backend policy of "+s.name, backendTrafficPolicyGVK, spec)
			}
			if s.clientPolicy != nil {
				spec := cloneMap(s.clientPolicy)
				attachKernelClientTrafficPolicyTarget(spec, wildcardListenerName)
				create("the client policy of "+s.name, clientTrafficPolicyGVK, spec)
			}
		}
	}
	if policies < 4 {
		t.Fatalf("only %d kernel policies were checked; the kernel's routes are not what this test thinks", policies)
	}

	// A component's policy, in a tenant's zone and outside the edge namespace,
	// with and without the token forwarded.
	zone := edgeZone{
		zoneNames: zoneNames{domain: "acme.example"}, realm: "tenant-acme", clientID: "gentian-edge-acme",
		secretName: "edge-acme-oidc", cookie: "gentian-acme-access", idCookie: "gentian-acme-id",
	}
	for _, authz := range []routeAuthz{
		{relation: "can_use", object: "app:acme/files"},
		{relation: "can_enter", object: "tenant:acme", forwardToken: true},
		{relation: "can_configure", object: "cluster:c1", keepClientToken: true},
		// An app's entry behind sign-in whose Authorization header is the
		// app's own, and the policy of a component one of whose hosts is.
		{relation: "can_use", object: "app:acme/flows", keepClientToken: true},
		{relation: "can_use", object: "app:acme/flows", idTokenSession: true},
	} {
		create(fmt.Sprintf("a zone policy (%+v)", authz), securityPolicyGVK,
			zoneSecurityPolicySpec("k.example", zone, "files-web", authz, servicesNamespace, "gentian-os-bouncer"))
	}

	// What an app's exposure may ask of its route, and the one client policy.
	app := backendTrafficPolicySpecFromIngressAnnotations(map[string]string{
		gentianov1alpha1.AnnotationIngressGatewayRequestTimeout: "600",
		gentianov1alpha1.AnnotationIngressGatewayBufferLimit:    "64Mi",
	})
	if app == nil {
		t.Fatal("the annotations made no policy")
	}
	attachBackendTrafficPolicyTarget(app, "files-web")
	create("an app's backend policy", backendTrafficPolicyGVK, app)
	keycloak := keycloakProxyBackendTrafficPolicySpec()
	attachBackendTrafficPolicyTarget(keycloak, kernelRouteKeycloakIDP)
	create("Keycloak's backend policy", backendTrafficPolicyGVK, keycloak)
	// The sign-in limits, keyed each way a cluster can be reached.
	for _, header := range []string{"", cloudflareClientAddressHeader} {
		realm := keycloakRealmBackendTrafficPolicySpec(header)
		if realm["rateLimit"] == nil {
			t.Fatal("the identity provider's public policy carries no rate limit")
		}
		attachBackendTrafficPolicyTarget(realm, kernelRouteKeycloakIDP)
		create("the identity provider's sign-in limit ("+header+")", backendTrafficPolicyGVK, realm)
		create("a sign-in sidecar's limit ("+header+")", backendTrafficPolicyGVK, map[string]interface{}{
			"targetRefs": []interface{}{map[string]interface{}{
				"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "notes-web-acs",
			}},
			"rateLimit": edgeSignInRateLimit("", header),
		})
	}
	slashes := escapedSlashesKeepUnchangedClientTrafficPolicySpec()
	attachKernelClientTrafficPolicyTarget(slashes, wildcardListenerName)
	create("the escaped-slashes client policy", clientTrafficPolicyGVK, slashes)

	// The EnvoyProxy, as the installer renders it for each network mode.
	for _, proxy := range []*unstructured.Unstructured{
		edgeEnvoyProxy(t, "ClusterIP"),
		edgeEnvoyProxy(t, "LoadBalancer", "--set", "nodeIp=203.0.113.10"),
	} {
		spec, _, _ := unstructured.NestedMap(proxy.Object, "spec")
		create("the edge EnvoyProxy", proxy.GroupVersionKind(), spec)
	}

	// And the check checks: a field the release does not have is refused.
	typo := kernelSecurityPolicySpec("k.example", "kernel", "kernel-argocd", routeAuthz{}, "gentian-os-bouncer")
	typo["oidc"].(map[string]interface{})["cookieConfg"] = map[string]interface{}{"sameSite": "Lax"}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(securityPolicyGVK)
	obj.SetName("typo")
	obj.SetNamespace(servicesNamespace)
	_ = unstructured.SetNestedField(obj.Object, jsonSafe(t, typo), "spec")
	if err := c.Create(ctx, obj, client.FieldValidation(metav1.FieldValidationStrict)); err == nil {
		t.Fatal("a misspelt field was accepted, so this test proves nothing")
	}
}

// jsonSafe round-trips a spec through JSON, which is what the client does to
// it and what turns Go ints into the numbers an unstructured object may hold.
func jsonSafe(t *testing.T, spec map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]interface{}{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
