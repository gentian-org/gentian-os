/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestRegistryMatchesPlatformsTable pins the two ends of the name together.
//
// kernel/platforms.yaml decides which credential the installer asks for; the
// registry decides which implementation gets built. They agree only because
// they use the same string, and nothing but this test says so — an ingress
// present in one and absent from the other is a cluster that either asks for a
// token it cannot use or builds an ingress nobody supplied a token for.
func TestRegistryMatchesPlatformsTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "kernel", "platforms.yaml"))
	if err != nil {
		t.Skipf("platforms.yaml unreadable: %v", err)
	}
	var doc struct {
		EdgeIngress map[string]struct {
			Credential map[string]interface{} `json:"credential"`
		} `json:"edgeIngress"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse platforms.yaml: %v", err)
	}

	registered := map[string]bool{}
	for _, n := range RegisteredEdgeIngresses() {
		registered[n] = true
	}

	for name := range doc.EdgeIngress {
		if name == "none" {
			continue // deliberately has no implementation
		}
		if !registered[name] {
			t.Errorf("edgeIngress %q is in platforms.yaml but no implementation is registered; "+
				"a cluster selecting it would be asked for a credential and then route nothing", name)
		}
	}
	for name := range registered {
		if _, ok := doc.EdgeIngress[name]; !ok {
			t.Errorf("edge ingress %q is registered but absent from platforms.yaml; "+
				"nothing would ask for its credential", name)
		}
	}
}

// TestBuildEdgeIngressNoneIsNil: static-ip is configured, not broken.
func TestBuildEdgeIngressNoneIsNil(t *testing.T) {
	for _, name := range []string{"", "none"} {
		ing, err := BuildEdgeIngress(name, EdgeIngressConfig{})
		if err != nil || ing != nil {
			t.Errorf("BuildEdgeIngress(%q) = (%v, %v), want (nil, nil)", name, ing, err)
		}
	}
}

// TestBuildEdgeIngressUnknownIsAnError: a typo must not read as static-ip.
//
// Returning nil would make a misspelled ingress indistinguishable from a
// cluster that correctly programs none — silent misconfiguration that looks
// like normal operation, which this platform has already paid for once.
func TestBuildEdgeIngressUnknownIsAnError(t *testing.T) {
	if _, err := BuildEdgeIngress("cf-tunnl", EdgeIngressConfig{Target: "x.cfargotunnel.com"}); err == nil {
		t.Error("a misspelled ingress must be an error, not a silent nil")
	}
}

// TestBuildEdgeIngressCfTunnel covers the two partial configurations: no
// tunnel yet is legitimate (cloudflared has not been created), no token for a
// tunnel that exists is not.
func TestBuildEdgeIngressCfTunnel(t *testing.T) {
	ing, err := BuildEdgeIngress("cf-tunnel", EdgeIngressConfig{Token: "t", Target: "abc.cfargotunnel.com"})
	if err != nil || ing == nil {
		t.Fatalf("fully configured: got (%v, %v)", ing, err)
	}
	if got := ing.DNSAnnotations()["external-dns.alpha.kubernetes.io/target"]; got != "abc.cfargotunnel.com" {
		t.Errorf("target annotation = %q", got)
	}
	if ing, err := BuildEdgeIngress("cf-tunnel", EdgeIngressConfig{Token: "t"}); err != nil || ing != nil {
		t.Errorf("no tunnel yet should be inert, got (%v, %v)", ing, err)
	}
	if _, err := BuildEdgeIngress("cf-tunnel", EdgeIngressConfig{Target: "abc.cfargotunnel.com"}); err == nil {
		t.Error("a tunnel with no token must be an error, not silence")
	}
}
