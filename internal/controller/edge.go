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

import "context"

// The edge is two questions with two different answers about who owns them.
//
//	what must a hostname RESOLVE to?   — external-dns. Every provider, always.
//	how does traffic REACH a service?  — EdgeIngress. Provider-specific, below.
//
// DNS is not in this file, and that is the point. external-dns already writes
// records for all eight providers in kernel/platforms.yaml, from the
// HTTPRoutes this operator writes (gateway-httproute) and from DNSEndpoint CRs
// for records with no HTTP object behind them, which is how mail already
// publishes. An operator that wrote records itself would be a ninth
// implementation of a solved problem, and it was: a Cloudflare-only one, which
// meant a Route 53 cluster had no DNS writer at all.
//
// What is genuinely missing on a tunnel cluster is not a writer but a TARGET.
// external-dns publishes what a Gateway resolves to, and a tunnelled Gateway
// has no address — traffic arrives through cloudflared, not through a
// LoadBalancer. So the ingress declares what its hostnames must point at, the
// operator stamps that on the kernel Gateway, and external-dns does the rest
// exactly as it does for a static-ip cluster.
//
// That is the whole seam. It is one annotation wide, and it keeps the DNS side
// generic while letting the ingress side be as vendor-specific as it must be:
// tunnels are a Cloudflare product, and inlets or frp would be their own
// implementation here rather than another value of one "tunnel" setting.

// EdgeIngress makes traffic for a hostname reach an in-cluster service, and
// tells external-dns what those hostnames must resolve to.
//
// nil is a valid edge: a static-ip cluster routes by LoadBalancer address, so
// there is nothing to program and external-dns reads the address from the
// Gateway itself. Callers use the helpers below rather than testing for nil.
type EdgeIngress interface {
	// EnsureRoute makes traffic for hostname reach service, idempotently.
	// service is an origin URL as the ingress understands it.
	EnsureRoute(ctx context.Context, hostname, service string) error

	// DeleteRoute removes the route for hostname. Absent is not an error.
	DeleteRoute(ctx context.Context, hostname string) error

	// DNSAnnotations are stamped on the kernel Gateway so external-dns
	// publishes records that reach this ingress.
	//
	// Returned rather than applied, because which annotations mean what is the
	// ingress's knowledge and the operator has no business holding a table of
	// them. It is also where provider-specific DNS behaviour belongs when an
	// ingress requires it: a Cloudflare tunnel needs its records proxied,
	// since cfargotunnel.com resolves to nothing a client could connect to,
	// and that is a fact about the tunnel rather than about DNS.
	DNSAnnotations() map[string]string
}

// edgeEnsureRoute programs a route when there is an ingress to program it on.
//
// A static-ip cluster has none and needs none: the LoadBalancer already routes
// by address. Callers say what they want to be true and this decides whether
// anything has to happen, rather than each of them testing for nil.
func edgeEnsureRoute(ctx context.Context, ing EdgeIngress, hostname, service string) error {
	if ing == nil {
		return nil
	}
	return ing.EnsureRoute(ctx, hostname, service)
}

// edgeDeleteRoute is edgeEnsureRoute's inverse.
func edgeDeleteRoute(ctx context.Context, ing EdgeIngress, hostname string) error {
	if ing == nil {
		return nil
	}
	return ing.DeleteRoute(ctx, hostname)
}

// edgeDNSAnnotations is what the kernel Gateway carries so external-dns can
// resolve this cluster's hostnames. Empty when no ingress needs one, which is
// the static-ip case: external-dns reads the Gateway's own address instead.
func edgeDNSAnnotations(ing EdgeIngress) map[string]string {
	if ing == nil {
		return nil
	}
	return ing.DNSAnnotations()
}
