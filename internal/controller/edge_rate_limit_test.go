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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

func clearEdgeLimitEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"EDGE_SIGN_IN_POSTS_PER_MINUTE", "PERIMETER_CLIENT_ADDRESS_HEADER", "EDGE_INGRESS", "NETWORK_MODE"} {
		t.Setenv(key, "")
	}
}

func rateLimitRules(t *testing.T, limit map[string]interface{}) []map[string]interface{} {
	t.Helper()
	if limit["type"] != "Local" {
		t.Fatalf("the limit is %v; only the local one needs no service beside the Gateway", limit["type"])
	}
	if _, global := limit["global"]; global {
		t.Fatal("a global limit needs a rate limit service, which this cluster does not run")
	}
	raw := limit["local"].(map[string]interface{})["rules"].([]interface{})
	out := make([]map[string]interface{}, len(raw))
	for i := range raw {
		out[i] = raw[i].(map[string]interface{})
	}
	return out
}

// Sixty posts a minute for each client address, keyed on the address the
// cluster can know: the connection's where the cluster has an address of
// its own, the tunnel's header where it is reached through the tunnel.
func TestTheSignInLimitIsPerClientAddress(t *testing.T) {
	clearEdgeLimitEnv(t)

	rules := rateLimitRules(t, edgeSignInRateLimit(keycloakSignInPostPattern, ""))
	if len(rules) != 2 {
		t.Fatalf("%d rules; one for IPv4 addresses and one for IPv6", len(rules))
	}
	for i, cidr := range []string{"0.0.0.0/0", "::/0"} {
		sel := rules[i]["clientSelectors"].([]interface{})[0].(map[string]interface{})
		src := sel["sourceCIDR"].(map[string]interface{})
		if src["type"] != "Distinct" || src["value"] != cidr {
			t.Errorf("rule %d keys on %v, want each address of %s its own bucket", i, src, cidr)
		}
		if _, ok := sel["headers"]; ok {
			t.Errorf("rule %d reads a header a caller can write", i)
		}
		if m := sel["methods"].([]interface{})[0].(map[string]interface{}); m["value"] != "POST" {
			t.Errorf("rule %d limits %v; pages and their assets are GETs and are not limited", i, m)
		}
		if p := sel["path"].(map[string]interface{}); p["type"] != "RegularExpression" || p["value"] != keycloakSignInPostPattern {
			t.Errorf("rule %d limits path %v", i, p)
		}
		if l := rules[i]["limit"].(map[string]interface{}); l["requests"] != int64(60) || l["unit"] != "Minute" {
			t.Errorf("rule %d allows %v, want 60 a minute", i, l)
		}
	}

	rules = rateLimitRules(t, edgeSignInRateLimit("", cloudflareClientAddressHeader))
	if len(rules) != 1 {
		t.Fatalf("%d rules behind the tunnel, want one", len(rules))
	}
	sel := rules[0]["clientSelectors"].([]interface{})[0].(map[string]interface{})
	h := sel["headers"].([]interface{})[0].(map[string]interface{})
	if h["name"] != "CF-Connecting-IP" || h["type"] != "Distinct" {
		t.Errorf("behind the tunnel the limit keys on %v; every request arrives from the tunnel's one address", h)
	}
	if _, ok := sel["sourceCIDR"]; ok {
		t.Error("behind the tunnel the connection's address is the tunnel's, and limiting it limits everybody at once")
	}
	if _, ok := sel["path"]; ok {
		t.Error("no path was asked for")
	}

	t.Setenv("EDGE_SIGN_IN_POSTS_PER_MINUTE", "600")
	rules = rateLimitRules(t, edgeSignInRateLimit("", cloudflareClientAddressHeader))
	if l := rules[0]["limit"].(map[string]interface{}); l["requests"] != int64(600) {
		t.Errorf("the administrator's value is not in force: %v", l)
	}
	t.Setenv("EDGE_SIGN_IN_POSTS_PER_MINUTE", "many")
	rules = rateLimitRules(t, edgeSignInRateLimit("", cloudflareClientAddressHeader))
	if l := rules[0]["limit"].(map[string]interface{}); l["requests"] != int64(60) {
		t.Errorf("an unusable value replaced the default: %v", l)
	}
	t.Setenv("EDGE_SIGN_IN_POSTS_PER_MINUTE", "0")
	if edgeSignInRateLimit("", "") != nil {
		t.Error("0 turns the limit off")
	}
	if _, has := keycloakRealmBackendTrafficPolicySpec("")["rateLimit"]; has {
		t.Error("with the limit off the identity provider's policy still carries one")
	}
}

// The identity provider's public route carries the limit, on what a sign-in
// page posts to; the administration console's route, behind a session, and
// every other kernel route carry none.
func TestOnlyTheIdentityProvidersPublicRouteIsLimited(t *testing.T) {
	clearEdgeLimitEnv(t)
	for _, s := range kernelHTTPRouteSpecs("k.example", nil, nil, nil, true, "c1", true, true, kernelFrontDoor{}) {
		if _, limited := s.policy["rateLimit"]; limited {
			t.Errorf("%s is limited where the routes are listed; the limit is set by the reconciler, for the one route", s.name)
		}
	}
	spec := keycloakRealmBackendTrafficPolicySpec("")
	if spec["rateLimit"] == nil || spec["connection"] == nil {
		t.Fatalf("the identity provider's policy is %v; want the proxy's settings and the limit", spec)
	}
	for _, path := range []string{
		"/auth/realms/kernel/login-actions/authenticate?session_code=abc&execution=1&client_id=x&tab_id=y",
		"/auth/realms/tenant-acme/login-actions/reset-credentials",
	} {
		if !matchesWhole(keycloakSignInPostPattern, path) {
			t.Errorf("a sign-in post is not limited: %s", path)
		}
	}
	for _, path := range []string{
		"/auth/realms/kernel/protocol/openid-connect/token",
		"/auth/realms/kernel/protocol/openid-connect/certs",
		"/auth/resources/abc/login/gentian/css/login.css",
		"/auth/realms/kernel/.well-known/openid-configuration",
	} {
		if matchesWhole(keycloakSignInPostPattern, path) {
			t.Errorf("a path servers call for everybody is limited: %s", path)
		}
	}
}

// The route that takes no session carries the limit too, as a policy of the
// component's own that goes when the sidecar does.
func TestTheSidecarsAnswerRouteIsLimited(t *testing.T) {
	clearEdgeLimitEnv(t)
	t.Setenv("NETWORK_MODE", "static-ip")
	h := signInHarness(t)
	ctx := context.Background()
	h.reconcile()

	name := signInACSRouteName("notes", "web")
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(backendTrafficPolicyGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: h.comp.Namespace, Name: name}, policy); err != nil {
		t.Fatalf("the answer's route has no rate limit: %v", err)
	}
	targets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if len(targets) != 1 || targets[0].(map[string]interface{})["name"] != name || targets[0].(map[string]interface{})["kind"] != "HTTPRoute" {
		t.Fatalf("the policy names %v, want the answer's route alone", targets)
	}
	limit, _, _ := unstructured.NestedMap(policy.Object, "spec", "rateLimit")
	if len(rateLimitRules(t, limit)) != 2 {
		t.Fatalf("the limit is %v", limit)
	}
	if owner := metav1.GetControllerOf(policy); owner == nil || owner.Kind != "Component" || owner.Name != "notes" {
		t.Fatalf("the policy is owned by %+v, so removing the app would leave it", owner)
	}

	// Switched off, the policy goes and the route stays.
	t.Setenv("EDGE_SIGN_IN_POSTS_PER_MINUTE", "0")
	h.reconcile()
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: h.comp.Namespace, Name: name}, policy); err == nil {
		t.Fatal("the limit was switched off and its policy is still there")
	}
	if h.route(name) == nil {
		t.Fatal("switching the limit off took the route with it")
	}
}

// matchesWhole is Envoy's reading of a path pattern: the whole of the path
// with its query, not a part of it.
func matchesWhole(pattern, s string) bool {
	return regexp.MustCompile("^(?:" + pattern + ")$").MatchString(s)
}

// What the operator writes is accepted by Envoy Gateway itself: its own
// translator, at the pinned release, turns each policy into Envoy
// configuration with a bucket per client address. A policy it refused would
// answer every request of its route with 500.
//
// Needs the release's egctl (EGCTL names it, or it is on PATH); skipped
// without, since it is a download and not a module.
func TestEnvoyGatewayAcceptsTheRateLimits(t *testing.T) {
	egctl := os.Getenv("EGCTL")
	if egctl == "" {
		found, err := exec.LookPath("egctl")
		if err != nil {
			t.Skip("egctl is not installed (set EGCTL): the policies were not put through Envoy Gateway's translator")
		}
		egctl = found
	}
	out, err := exec.Command(egctl, "version").CombinedOutput()
	if err != nil {
		t.Skipf("egctl does not run: %v", err)
	}
	pin, err := os.ReadFile(filepath.Join(envoyGatewayCRDs, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), strings.TrimSpace(string(pin))) {
		t.Skipf("egctl is %s, the pinned release is %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(pin)))
	}

	clearEdgeLimitEnv(t)
	for mode, header := range map[string]string{"static-ip": "", "tunnel": cloudflareClientAddressHeader} {
		wantKey := "remote-address"
		if header != "" {
			wantKey = header
		}
		spec := keycloakRealmBackendTrafficPolicySpec(header)
		attachBackendTrafficPolicyTarget(spec, kernelRouteKeycloakIDP)
		policy, err := yaml.Marshal(map[string]interface{}{
			"apiVersion": "gateway.envoyproxy.io/v1alpha1", "kind": "BackendTrafficPolicy",
			"metadata": map[string]interface{}{"name": "btp-kernel-idp", "namespace": "kernel-edge"},
			"spec":     spec,
		})
		if err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(t.TempDir(), "in.yaml")
		if err := os.WriteFile(input, append([]byte(rateLimitFixture), policy...), 0o600); err != nil {
			t.Fatal(err)
		}
		status, err := exec.Command(egctl, "x", "translate", "--from", "gateway-api", "--to", "gateway-api", "-f", input, "-o", "json").Output()
		if err != nil {
			t.Fatalf("%s: egctl: %v", mode, err)
		}
		var translated struct {
			BackendTrafficPolicies []struct {
				Status struct {
					Ancestors []struct {
						Conditions []metav1.Condition `json:"conditions"`
					} `json:"ancestors"`
				} `json:"status"`
			} `json:"backendTrafficPolicies"`
		}
		if err := json.Unmarshal(status, &translated); err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, status)
		}
		accepted := false
		for _, p := range translated.BackendTrafficPolicies {
			for _, a := range p.Status.Ancestors {
				for _, c := range a.Conditions {
					if c.Type == "Accepted" {
						accepted = c.Status == metav1.ConditionTrue
						if !accepted {
							t.Errorf("%s: Envoy Gateway refuses the policy: %s", mode, c.Message)
						}
					}
				}
			}
		}
		if !accepted {
			t.Fatalf("%s: the policy was not accepted:\n%s", mode, status)
		}
		xds, err := exec.Command(egctl, "x", "translate", "--from", "gateway-api", "--to", "xds", "-t", "route", "-f", input, "-o", "json").Output()
		if err != nil {
			t.Fatalf("%s: egctl: %v", mode, err)
		}
		for _, want := range []string{"envoy.filters.http.local_ratelimit", wantKey, `"maxTokens":60`, `"fillInterval":"60s"`, "login-actions"} {
			if !strings.Contains(strings.ReplaceAll(string(xds), " ", ""), strings.ReplaceAll(want, " ", "")) {
				t.Errorf("%s: Envoy's configuration lacks %s", mode, want)
			}
		}
	}
}

const rateLimitFixture = `apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: gentian-envoy}
spec: {controllerName: gateway.envoyproxy.io/gatewayclass-controller}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: perimeter, namespace: kernel-edge}
spec:
  gatewayClassName: gentian-envoy
  listeners:
  - {name: http-id, protocol: HTTP, port: 80, hostname: id.k.example}
---
apiVersion: v1
kind: Service
metadata: {name: keycloak, namespace: kernel-edge}
spec: {ports: [{port: 8080, protocol: TCP}], clusterIP: 10.0.0.5}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: kernel-idp, namespace: kernel-edge}
spec:
  parentRefs: [{name: perimeter}]
  hostnames: [id.k.example]
  rules:
  - matches: [{path: {type: PathPrefix, value: /auth/realms/}}]
    backendRefs: [{name: keycloak, port: 8080}]
---
`
