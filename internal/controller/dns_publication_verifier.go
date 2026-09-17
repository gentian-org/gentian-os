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

	for i := range list.Items {
		ep := &list.Items[i]
		var missing []string
		for _, rec := range dnsRecordsOf(ep) {
			served, known := v.served(ctx, rec)
			if !known {
				continue
			}
			k := rec.key()
			labels := prometheus.Labels{"namespace": rec.Namespace, "endpoint": rec.Endpoint, "name": rec.Name, "type": rec.Type}
			if served {
				dnsRecordPublished.With(labels).Set(1)
				if v.reported[k] {
					logger.Info("DNS record is now published", "endpoint", rec.Endpoint, "name", rec.Name, "type", rec.Type)
				}
				delete(v.missingSince, k)
				delete(v.reported, k)
				continue
			}
			first, seen := v.missingSince[k]
			if !seen {
				v.missingSince[k] = now
				continue
			}
			if now.Sub(first) < grace {
				continue
			}
			// Only past the grace period does a missing record count, and only
			// then does the gauge go to 0: a gauge that dips on every new record
			// while it propagates is an alert that teaches people to ignore it.
			dnsRecordPublished.With(labels).Set(0)
			missing = append(missing, fmt.Sprintf("%s %s", rec.Type, rec.Name))
			if !v.reported[k] {
				logger.Error(nil, "DNS record requested but not published",
					"endpoint", rec.Namespace+"/"+rec.Endpoint, "name", rec.Name, "type", rec.Type,
					"targets", rec.Targets, "missingFor", now.Sub(first).Round(time.Second).String())
				v.reported[k] = true
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
}

// served reports whether rec is published. known is false for record types this
// does not check and for lookups that failed for reasons other than absence —
// a resolver outage must not read as every record vanishing at once.
func (v *DNSPublicationVerifier) served(ctx context.Context, rec dnsRecord) (served, known bool) {
	switch rec.Type {
	case "A", "AAAA", "TXT", "MX":
	default:
		return false, false
	}
	resolve := v.Resolve
	if resolve == nil {
		resolve = authoritativeResolver
	}
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	res, err := resolve(ctx, rec.Name)
	if err != nil {
		return false, false
	}
	switch rec.Type {
	case "A", "AAAA":
		got, err := res.LookupHost(ctx, rec.Name)
		if err != nil {
			return false, isNotFound(err)
		}
		return containsAll(got, rec.Targets, normaliseHost), true
	case "TXT":
		got, err := res.LookupTXT(ctx, rec.Name)
		if err != nil {
			return false, isNotFound(err)
		}
		return containsAll(got, rec.Targets, normaliseTXT), true
	case "MX":
		mx, err := res.LookupMX(ctx, rec.Name)
		if err != nil {
			return false, isNotFound(err)
		}
		got := make([]string, 0, len(mx))
		for _, m := range mx {
			got = append(got, strconv.Itoa(int(m.Pref))+" "+m.Host)
		}
		return containsAll(got, rec.Targets, normaliseMX), true
	}
	return false, false
}

// isNotFound distinguishes "the name has no such record" — which is the answer
// being checked for — from a lookup that could not be completed.
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
