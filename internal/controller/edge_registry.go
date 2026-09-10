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
	"fmt"
	"sort"
)

// Which EdgeIngress a cluster runs, chosen by name.
//
// The name is the key in kernel/platforms.yaml's edgeIngress table, so the
// catalogue that decides which credential to ask for and the code that decides
// which implementation to build agree by construction rather than by two people
// remembering the same string.
//
// Adding an ingress is: an entry in that table, an EdgeIngress implementation,
// and a Register call. Nothing in the operator's startup, no reconciler and no
// caller changes — they hold the interface, never a concrete type.

// EdgeIngressConfig is everything an implementation may need, read once from
// the environment and passed to whichever one is selected.
//
// A single struct rather than a per-implementation signature: the alternative
// is each new ingress editing the startup path to thread its own values, which
// is the coupling this registry exists to remove. Fields no implementation
// reads cost nothing.
type EdgeIngressConfig struct {
	// Token authorises the ingress's own API, whatever that is.
	Token string
	// Target is the address or hostname traffic arrives at, when the ingress
	// is told rather than asked — a tunnel CNAME, an exit node's address.
	Target string
	// Zone and Account scope the credential for providers that need it.
	Zone    string
	Account string
}

// EdgeIngressFactory builds one implementation from config. Returning a nil
// EdgeIngress with a nil error means "correctly configured to do nothing" —
// the static-ip case, where the LoadBalancer already routes.
type EdgeIngressFactory func(EdgeIngressConfig) (EdgeIngress, error)

var edgeIngressRegistry = map[string]EdgeIngressFactory{}

// RegisterEdgeIngress adds an implementation under the name platforms.yaml
// uses. Panics on a duplicate: two factories for one name is a build-time
// mistake, and discovering it at startup on a customer's cluster is worse than
// failing here.
func RegisterEdgeIngress(name string, f EdgeIngressFactory) {
	if _, dup := edgeIngressRegistry[name]; dup {
		panic(fmt.Sprintf("edge ingress %q registered twice", name))
	}
	edgeIngressRegistry[name] = f
}

// BuildEdgeIngress returns the implementation registered under name.
//
// "none" and "" are valid and yield nil: a static-ip cluster programs no
// ingress, and nil is a supported EdgeIngress everywhere (see edge.go).
//
// An unknown name is an error rather than a silent nil. A typo'd ingress that
// quietly disabled routing would look exactly like a correctly configured
// static-ip cluster, and this platform has already paid for one silent
// misconfiguration that read as normal operation.
func BuildEdgeIngress(name string, cfg EdgeIngressConfig) (EdgeIngress, error) {
	if name == "" || name == "none" {
		return nil, nil
	}
	f, ok := edgeIngressRegistry[name]
	if !ok {
		return nil, fmt.Errorf(
			"unknown edge ingress %q; kernel/platforms.yaml edgeIngress knows %v",
			name, RegisteredEdgeIngresses())
	}
	return f(cfg)
}

// RegisteredEdgeIngresses is the sorted list of names, for error messages and
// tests that assert the table and the registry agree.
func RegisteredEdgeIngresses() []string {
	names := make([]string, 0, len(edgeIngressRegistry))
	for n := range edgeIngressRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() {
	// The Cloudflare tunnel. Its own file holds everything Cloudflare-specific;
	// this line is the whole of its integration with the rest of the operator.
	RegisterEdgeIngress("cf-tunnel", func(cfg EdgeIngressConfig) (EdgeIngress, error) {
		if cfg.Target == "" {
			// Configured by name but with nothing to point at. Not an error:
			// the tunnel CNAME is resolved from the cluster and is legitimately
			// absent before cloudflared has been created.
			return nil, nil
		}
		if cfg.Token == "" {
			return nil, fmt.Errorf("edge ingress cf-tunnel has a tunnel (%s) but no token to configure it", cfg.Target)
		}
		return NewCloudflareTunnelIngress(cfg.Token, cfg.Zone, cfg.Target, cfg.Account), nil
	})
}
