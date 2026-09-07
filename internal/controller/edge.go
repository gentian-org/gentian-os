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

// The cluster's edge is two questions, and Cloudflare is the reason they were
// ever one type.
//
//	how does traffic for a hostname REACH a service?   — EdgeIngress
//	what must that hostname RESOLVE to?                — EdgeDNSWriter
//
// With Cloudflare both are the same vendor, the same account and (by default)
// the same token, so a single client answering both reads as natural. It is a
// coincidence of one provider. With inlets, frp, or a WireGuard exit node the
// ingress is a host you run and DNS is whoever serves the zone — different
// vendors, different credentials, no shared API. static-ip mode already shows
// the split inside this codebase today: no tunnel at all, DNS pointing straight
// at a LoadBalancer address.
//
// Keeping them apart costs one indirection now and is what makes a second edge
// possible later without touching the DNS side, or a second DNS provider
// without touching the tunnel.

// EdgeTarget is what a hostname must resolve to for traffic to arrive: the
// value a DNS record carries, and the record type that carries it.
//
// This is the whole contract between the two halves. EdgeIngress decides it —
// a Cloudflare tunnel answers with a CNAME to <id>.cfargotunnel.com, a
// LoadBalancer with an A record to its address — and EdgeDNSWriter writes
// whatever it is handed without knowing which produced it.
type EdgeTarget struct {
	// Type is a DNS record type: "CNAME" or "A".
	Type string
	// Value is the record's content: a hostname for CNAME, an address for A.
	Value string
	// Proxied asks the DNS provider to terminate traffic at its own edge
	// rather than hand the client this address to connect to directly —
	// Cloudflare's orange cloud, and the equivalent on any CDN-backed DNS.
	//
	// It belongs to the ingress, not to the writer, because only the ingress
	// knows whether it requires one: a cfargotunnel.com CNAME does not work
	// unproxied, since the tunnel has no address a client could reach. A
	// LoadBalancer address is the opposite case. A provider with no such
	// concept ignores it.
	Proxied bool
}

// IsZero reports whether an ingress declined to name a target. A static-ip
// cluster whose address is not yet allocated is the ordinary case, and the
// caller skips the DNS write rather than writing an empty record.
func (t EdgeTarget) IsZero() bool { return t.Type == "" || t.Value == "" }

// EdgeDNSWriter makes a hostname resolve to the cluster's edge.
//
// Deliberately ignorant of tunnels. Every provider on the dnsProviders list in
// kernel/platforms.yaml can implement this — it is a CNAME or an A record and
// nothing else — which is why external-dns already serves as a generic
// implementation of exactly this interface for eight providers at once.
type EdgeDNSWriter interface {
	// EnsureRecord makes hostname resolve to target, idempotently.
	EnsureRecord(ctx context.Context, hostname string, target EdgeTarget) error
	// DeleteRecord removes the records for hostname. Absent is not an error.
	DeleteRecord(ctx context.Context, hostname string) error
}

// EdgeIngress makes traffic for a hostname reach an in-cluster service.
//
// Implementations are edge-shaped rather than DNS-shaped: a Cloudflare tunnel
// programs ingress rules on a remotely-managed tunnel; inlets would configure
// an exit node; a static-ip cluster does nothing at all here, because a
// LoadBalancer already routes by address and only DNS is missing.
type EdgeIngress interface {
	// EnsureRoute makes traffic for hostname reach service, idempotently.
	// service is an origin URL as the ingress understands it.
	EnsureRoute(ctx context.Context, hostname, service string) error
	// DeleteRoute removes the route for hostname. Absent is not an error.
	DeleteRoute(ctx context.Context, hostname string) error
	// Target is what DNS must point at to reach this ingress.
	Target() EdgeTarget
}

// Edge is the pair, as a reconciler holds them.
//
// Either may be nil and they are nil independently: a cluster can publish DNS
// with no tunnel (static-ip), and a cluster whose DNS an operator maintains by
// hand can have a tunnel with no writer. Callers check the half they need
// rather than assuming both arrived together.
type Edge struct {
	DNS     EdgeDNSWriter
	Ingress EdgeIngress
}

// EnsureHostname is the invariant the kernel path got wrong: a route to a
// hostname nothing resolves is unreachable, and a record pointing at an
// ingress that does not route it is a name that answers and then fails. They
// are one operation, so there is one method that does both.
func (e *Edge) EnsureHostname(ctx context.Context, hostname, service string) error {
	if e == nil {
		return nil
	}
	if e.Ingress != nil {
		if err := e.Ingress.EnsureRoute(ctx, hostname, service); err != nil {
			return err
		}
	}
	if e.DNS == nil {
		return nil
	}
	// The target comes from the ingress when there is one. Without an ingress
	// there is nothing to point at from here, and DNS is someone else's job —
	// external-dns, or an operator by hand.
	if e.Ingress == nil {
		return nil
	}
	target := e.Ingress.Target()
	if target.IsZero() {
		return nil
	}
	return e.DNS.EnsureRecord(ctx, hostname, target)
}

// DeleteHostname reverses EnsureHostname. DNS first: a name that still
// resolves to an ingress that no longer routes it serves errors, while a route
// nobody can reach is merely inert.
func (e *Edge) DeleteHostname(ctx context.Context, hostname string) error {
	if e == nil {
		return nil
	}
	if e.DNS != nil {
		if err := e.DNS.DeleteRecord(ctx, hostname); err != nil {
			return err
		}
	}
	if e.Ingress != nil {
		return e.Ingress.DeleteRoute(ctx, hostname)
	}
	return nil
}
