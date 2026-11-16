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
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DNS records the platform asked for, checked against what the internet serves.
//
// Writing a DNSEndpoint is a request, not a result. external-dns decides whether
// to act on it, and it can decline silently: it creates at a name only when it
// owns that name, so a record created by hand before install — or an ownership
// marker it cannot write, as the zone apex was under a prefix without
// %{record_type} — makes it skip every create there while logging "All records
// are already up to date". On this platform that left the kernel domain with
// DKIM but no SPF and no MX for as long as anyone looked, and the first sign was
// a receiving server's verdict on mail, not anything the platform reported.
//
// Ownership was that cause. A scoped API token, a domain filter or a provider
// error would be equally invisible, so this checks the outcome rather than any
// one mechanism: for every record in every DNSEndpoint, does the zone's
// authoritative server answer with it?
const (
	// Long enough for external-dns's sync interval plus the record TTL the
	// platform publishes, so a record that is merely propagating is not reported.
	dnsPublicationGrace = 10 * time.Minute
	// DNS lookups are network I/O with no useful upper bound on latency, which
	// is why this runs on its own ticker instead of inside a tenant reconcile.
	dnsPublicationInterval = 5 * time.Minute
	dnsLookupTimeout       = 10 * time.Second
)

var dnsRecordPublished = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "gentianos_dns_record_published",
	Help: "Whether a record requested through a DNSEndpoint is served by the zone's " +
		"authoritative nameservers (1) or not (0). Unset until checked.",
}, []string{"namespace", "endpoint", "name", "type"})

// dnsResolver is the lookups the verifier needs, so tests can answer without a
// network.
type dnsResolver interface {
	LookupHost(ctx context.Context, name string) ([]string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// dnsRecord is one requested record, flattened out of a DNSEndpoint.
type dnsRecord struct {
	Namespace, Endpoint, Name, Type string
	Targets                         []string
}

func (d dnsRecord) key() string {
	return d.Namespace + "/" + d.Endpoint + "/" + d.Type + "/" + d.Name
}

// DNSPublicationVerifier periodically checks that DNSEndpoint records are served.
type DNSPublicationVerifier struct {
	Client   client.Client
	Recorder record.EventRecorder
	Interval time.Duration
	Grace    time.Duration
	// Resolve returns a resolver that asks the authoritative servers for name.
	// Nil means the real one; tests substitute their own.
	Resolve func(ctx context.Context, name string) (dnsResolver, error)

	// KernelDomain, TenancyMode and KernelRealm say which domains the
	// cluster serves, so the result can be told by domain
	// (dns_domain_status.go). Namespace is where that is written; empty
	// writes nothing.
	KernelDomain, TenancyMode, KernelRealm string
	Namespace                              string

	mu           sync.Mutex
	missingSince map[string]time.Time
	reported     map[string]bool
}

// NeedLeaderElection keeps the check to one replica, so an unpublished record
// produces one Event rather than one per operator Pod.
func (v *DNSPublicationVerifier) NeedLeaderElection() bool { return true }

// Start runs until the manager stops.
func (v *DNSPublicationVerifier) Start(ctx context.Context) error {
	interval := v.Interval
	if interval <= 0 {
		interval = dnsPublicationInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		v.CheckOnce(ctx, time.Now())
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// CheckOnce verifies every requested record once. Exported for tests.
func (v *DNSPublicationVerifier) CheckOnce(ctx context.Context, now time.Time) {
	logger := log.FromContext(ctx).WithName("dns-publication")
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(dnsEndpointGVK.GroupVersion().WithKind("DNSEndpointList"))
	if err := v.Client.List(ctx, list); err != nil {
		// No external-dns, no DNSEndpoints, nothing to verify — the same reading
		// the publishing side takes of an absent CRD.
		logger.V(1).Info("cannot list DNSEndpoints", "error", err.Error())
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.missingSince == nil {
		v.missingSince = map[string]time.Time{}
		v.reported = map[string]bool{}
	}
	grace := v.Grace
	if grace <= 0 {
		grace = dnsPublicationGrace
	}

	var results []dnsRecordStatus
	// check looks one record up and keeps its state; late is a record past
	// the grace period that is still not served.
	check := func(rec dnsRecord) (late bool) {
		if !dnsCheckedType(rec.Type) {
			// A CNAME, say: not looked up, and so not listed either.
			return false
		}
		got, served, known := v.observe(ctx, rec)
		status := dnsRecordStatus{record: rec, Got: got}
		defer func() { results = append(results, status) }()
		if !known {
			status.State = dnsStateUnknown
			return false
		}
		k := rec.key()
		labels := prometheus.Labels{"namespace": rec.Namespace, "endpoint": rec.Endpoint, "name": rec.Name, "type": rec.Type}
		if served {
			status.State = dnsStatePresent
			dnsRecordPublished.With(labels).Set(1)
			if v.reported[k] {
				logger.Info("DNS record is now published", "endpoint", rec.Endpoint, "name", rec.Name, "type", rec.Type)
			}
			delete(v.missingSince, k)
			delete(v.reported, k)
			return false
		}
		first, seen := v.missingSince[k]
		if !seen {
			v.missingSince[k] = now
			first = now
		}
		status.Since = first
		if now.Sub(first) < grace {
			status.State = dnsStatePending
			return false
		}
		status.State = dnsStateMissing
		if heldByOther(rec, got) {
			status.State = dnsStateHeldByOther
		}
		// Only past the grace period does a missing record count, and only
		// then does the gauge go to 0: a gauge that dips on every new record
		// while it propagates is an alert that teaches people to ignore it.
		dnsRecordPublished.With(labels).Set(0)
		if !v.reported[k] {
			logger.Error(nil, "DNS record requested but not published",
				"endpoint", rec.Namespace+"/"+rec.Endpoint, "name", rec.Name, "type", rec.Type,
				"targets", rec.Targets, "served", got, "missingFor", now.Sub(first).Round(time.Second).String())
			v.reported[k] = true
		}
		return true
	}

	for i := range list.Items {
		ep := &list.Items[i]
		var missing []string
		for _, rec := range dnsRecordsOf(ep) {
			if check(rec) {
				missing = append(missing, fmt.Sprintf("%s %s", rec.Type, rec.Name))
			}
		}
		if len(missing) > 0 && v.Recorder != nil {
			sort.Strings(missing)
			v.Recorder.Eventf(ep, corev1.EventTypeWarning, "DNSRecordNotPublished",
				"Requested but not served by the authoritative nameservers after %s: %s. "+
					"If external-dns reports all records up to date, a record at that name it does not own "+
					"is blocking the create (see scripts/tools/extdns-reown.sh).",
				grace, strings.Join(missing, ", "))
		}
	}

	// The bare names and www hosts the cluster routes, which external-dns
	// publishes from the routes rather than from a DNSEndpoint.
	domains := v.servedDomains(ctx)
	for _, rec := range v.webRecords(ctx, domains, results) {
		check(rec)
	}
	v.writeDomainStatus(ctx, domains, results, now)
}

// observe asks the zone's authoritative server for one record: what it
// serves at that name and type, whether that includes every target asked
// for, and whether the answer is known at all. known is false for record
// types this does not check and for lookups that failed for reasons other
// than absence -- a resolver outage must not read as every record vanishing
// at once.
func (v *DNSPublicationVerifier) observe(ctx context.Context, rec dnsRecord) (got []string, served, known bool) {
	if !dnsCheckedType(rec.Type) {
		return nil, false, false
	}
	resolve := v.Resolve
	if resolve == nil {
		resolve = authoritativeResolver
	}
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	res, err := resolve(ctx, rec.Name)
	if err != nil {
		return nil, false, false
	}
	norm := normaliseHost
	switch rec.Type {
	case "A", "AAAA":
		got, err = res.LookupHost(ctx, rec.Name)
	case "TXT":
		got, err = res.LookupTXT(ctx, rec.Name)
		norm = normaliseTXT
	case "MX":
		var mx []*net.MX
		mx, err = res.LookupMX(ctx, rec.Name)
		for _, m := range mx {
			got = append(got, strconv.Itoa(int(m.Pref))+" "+m.Host)
		}
		norm = normaliseMX
	}
	if err != nil {
		return nil, false, isNotFound(err)
	}
	return got, containsAll(got, rec.Targets, norm), true
}

// dnsCheckedType says whether records of a type are looked up.
func dnsCheckedType(typ string) bool {
	switch typ {
	case "A", "AAAA", "TXT", "MX":
		return true
	}
	return false
}

func isNotFound(err error) bool {
	if dnsErr, ok := err.(*net.DNSError); ok {
		return dnsErr.IsNotFound
	}
	return false
}

func containsAll(got, want []string, norm func(string) string) bool {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[norm(g)] = true
	}
	for _, w := range want {
		if !have[norm(w)] {
			return false
		}
	}
	return true
}

func normaliseHost(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// TXT content comes back without the quotes a zone file shows, and a long value
// split into 255-byte strings is joined again by the resolver.
func normaliseTXT(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

// "0 mail.example.com" and "0 mail.example.com." are the same MX.
func normaliseMX(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// dnsRecordsOf flattens a DNSEndpoint's spec.endpoints.
func dnsRecordsOf(ep *unstructured.Unstructured) []dnsRecord {
	raw, found, err := unstructured.NestedSlice(ep.Object, "spec", "endpoints")
	if err != nil || !found {
		return nil
	}
	out := make([]dnsRecord, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["dnsName"].(string)
		typ, _ := m["recordType"].(string)
		if name == "" || typ == "" {
			continue
		}
		var targets []string
		if ts, ok := m["targets"].([]interface{}); ok {
			for _, t := range ts {
				if s, ok := t.(string); ok {
					targets = append(targets, s)
				}
			}
		}
		out = append(out, dnsRecord{
			Namespace: ep.GetNamespace(), Endpoint: ep.GetName(),
			Name: strings.TrimSuffix(name, "."), Type: strings.ToUpper(typ), Targets: targets,
		})
	}
	return out
}

// authoritativeResolver returns a resolver that asks the zone's own nameservers.
//
// Public resolvers would do for a record that has existed for a while, but they
// cache NXDOMAIN too, and a record checked shortly after it was created could be
// reported missing for the negative-cache lifetime even though it is live.
// Asking the authority answers the question actually being asked.
func authoritativeResolver(ctx context.Context, name string) (dnsResolver, error) {
	ns, err := zoneNameservers(ctx, name)
	if err != nil {
		return nil, err
	}
	server := net.JoinHostPort(strings.TrimSuffix(ns, "."), "53")
	dialer := &net.Dialer{Timeout: dnsLookupTimeout}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, server)
		},
	}, nil
}

// zoneNameservers finds the nameservers of the closest enclosing zone by
// walking up from the name, which works for the apex and any depth below it.
func zoneNameservers(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		nss, err := net.DefaultResolver.LookupNS(ctx, strings.Join(labels[i:], "."))
		if err == nil && len(nss) > 0 {
			return nss[0].Host, nil
		}
	}
	return "", fmt.Errorf("no enclosing zone found for %s", name)
}
