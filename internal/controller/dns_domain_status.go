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
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
)

// The DNS records each served domain needs, and whether the internet has them
// (routing.md §3a).
//
// The verifier already knew when a record the cluster asked for was not
// served, but said so only in its log, a metric and an event on a DNSEndpoint
// nobody looks at. A domain whose bare name had no address for days because
// somebody else's mail records sat on it was found by a person whose browser
// could not reach it. This puts the same answer where an operator looks: one
// list per domain the cluster serves, read by the director
// (GET /v1/clusters/{c}/domains) for the Operations Console and
// `kubectl gentian domains`.

// DNSStatusConfigMapName is the ConfigMap the status is written to, in the
// operator's namespace, under DNSStatusKey.
const (
	DNSStatusConfigMapName = "gentian-dns-status"
	DNSStatusKey           = "status.json"
)

// The state of one record.
const (
	// dnsStatePresent: the zone's servers answer with every value asked for.
	dnsStatePresent = "present"
	// dnsStatePending: not answered yet, within the grace period a new
	// record needs to propagate.
	dnsStatePending = "pending"
	// dnsStateMissing: past the grace period, and nothing of its kind is
	// served at that name.
	dnsStateMissing = "missing"
	// dnsStateHeldByOther: past the grace period, and something else of its
	// kind is served at that name -- another provider's MX or SPF, an
	// address that is not the cluster's. external-dns never overwrites a
	// record it does not own, so this one waits for a person.
	dnsStateHeldByOther = "heldByOther"
	// dnsStateUnknown: the lookup failed for another reason than absence.
	dnsStateUnknown = "unknown"
)

// dnsRecordStatus is one record and what was found.
type dnsRecordStatus struct {
	record dnsRecord
	Got    []string
	State  string
	Since  time.Time
}

// DNSStatus is what is written, and what the director passes on.
type DNSStatus struct {
	CheckedAt string            `json:"checkedAt"`
	Domains   []DNSDomainStatus `json:"domains"`
}

// DNSDomainStatus is one served domain: the cluster's own (no tenant) or a
// tenant's zone or domain.
type DNSDomainStatus struct {
	Domain  string            `json:"domain"`
	Tenant  string            `json:"tenant,omitempty"`
	Records []DNSRecordReport `json:"records"`
}

// DNSRecordReport is one record of a domain.
type DNSRecordReport struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Want is what the cluster asked for, Got what the zone's servers
	// answer at that name and type.
	Want  []string `json:"want"`
	Got   []string `json:"got,omitempty"`
	State string   `json:"state"`
	// Since is when it was first seen not served; empty when it is.
	Since string `json:"since,omitempty"`
}

// servedDomain is one domain the cluster serves, and whose it is.
type servedDomain struct {
	domain, tenant string
}

// servedDomains are the cluster's own domain and every tenant's.
func (v *DNSPublicationVerifier) servedDomains(ctx context.Context) []servedDomain {
	if v.KernelDomain == "" {
		return nil
	}
	out := []servedDomain{{domain: v.KernelDomain}}
	tenants := &gentianov1alpha1.TenantList{}
	if err := v.Client.List(ctx, tenants); err != nil {
		log.FromContext(ctx).V(1).Info("cannot list tenants for the DNS status", "error", err.Error())
		return out
	}
	seen := map[string]bool{v.KernelDomain: true}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp != nil {
			continue
		}
		d := addresses.ZoneOf(t, v.KernelDomain, v.TenancyMode, v.KernelRealm).Domain
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, servedDomain{domain: d, tenant: t.Name})
	}
	return out
}

// webRecords are the address records of the bare names and www hosts the
// cluster routes, where no DNSEndpoint already asks for one. On a cluster
// with a public address, external-dns publishes them from the routes, with
// the Gateway's address as the value; that address is what is asked for
// here. A Gateway with no address (a tunnel) has its hosts in a DNSEndpoint
// instead, and adds nothing.
func (v *DNSPublicationVerifier) webRecords(ctx context.Context, domains []servedDomain, have []dnsRecordStatus) []dnsRecord {
	if len(domains) == 0 {
		return nil
	}
	gateways := &gatewayv1.GatewayList{}
	if err := v.Client.List(ctx, gateways, client.InNamespace(servicesNamespace)); err != nil {
		return nil
	}
	var targets []string
	seenAddr := map[string]bool{}
	for i := range gateways.Items {
		for _, a := range gateways.Items[i].Status.Addresses {
			ip := net.ParseIP(a.Value)
			if ip == nil || ip.To4() == nil || seenAddr[a.Value] {
				continue
			}
			seenAddr[a.Value] = true
			targets = append(targets, a.Value)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sort.Strings(targets)
	routes := &gatewayv1.HTTPRouteList{}
	if err := v.Client.List(ctx, routes); err != nil {
		return nil
	}
	routed := map[string]bool{}
	for i := range routes.Items {
		for _, h := range routes.Items[i].Spec.Hostnames {
			routed[strings.ToLower(string(h))] = true
		}
	}
	// A name a DNSEndpoint already gives an address (a tunnel's CNAME, the
	// mail host's A) is that endpoint's to check.
	asked := map[string]bool{}
	for _, r := range have {
		switch r.record.Type {
		case "A", "AAAA", "CNAME":
			asked[r.record.Name] = true
		}
	}
	var out []dnsRecord
	for _, d := range domains {
		for _, name := range []string{d.domain, "www." + d.domain} {
			if !routed[name] || asked[name] {
				continue
			}
			out = append(out, dnsRecord{Endpoint: "routes", Name: name, Type: "A", Targets: targets})
		}
	}
	return out
}

// heldByOther says a record that is not served has something else of its
// kind at its name. For TXT that is a value of the same kind -- another SPF,
// DKIM key or DMARC policy -- since unrelated TXT records (a site
// verification) live beside the cluster's at the same name.
func heldByOther(rec dnsRecord, got []string) bool {
	if len(got) == 0 {
		return false
	}
	if rec.Type != "TXT" {
		return true
	}
	for _, want := range rec.Targets {
		tag := txtTag(want)
		if tag == "" {
			return true
		}
		for _, g := range got {
			if txtTag(g) == tag {
				return true
			}
		}
	}
	return false
}

// txtTag is the kind of a TXT value: its version tag (v=spf1, v=DKIM1,
// v=DMARC1), or "" for anything else.
func txtTag(value string) string {
	value = normaliseTXT(value)
	if !strings.HasPrefix(strings.ToLower(value), "v=") {
		return ""
	}
	end := strings.IndexAny(value, "; ")
	if end < 0 {
		end = len(value)
	}
	return strings.ToLower(value[:end])
}

// domainOf is the served domain a name belongs to: the longest one it is or
// is under.
func domainOf(name string, domains []servedDomain) (servedDomain, bool) {
	best, found := servedDomain{}, false
	for _, d := range domains {
		if (name == d.domain || strings.HasSuffix(name, "."+d.domain)) && len(d.domain) > len(best.domain) {
			best, found = d, true
		}
	}
	return best, found
}

// domainStatus sorts the results into one list per served domain. A record
// under no served domain is listed under its own name.
func domainStatus(domains []servedDomain, results []dnsRecordStatus, now time.Time) DNSStatus {
	by := map[string]*DNSDomainStatus{}
	var order []string
	for _, d := range domains {
		by[d.domain] = &DNSDomainStatus{Domain: d.domain, Tenant: d.tenant, Records: []DNSRecordReport{}}
		order = append(order, d.domain)
	}
	for _, r := range results {
		d, ok := domainOf(r.record.Name, domains)
		if !ok {
			d = servedDomain{domain: r.record.Name}
		}
		entry := by[d.domain]
		if entry == nil {
			entry = &DNSDomainStatus{Domain: d.domain, Records: []DNSRecordReport{}}
			by[d.domain] = entry
			order = append(order, d.domain)
		}
		report := DNSRecordReport{
			Name: r.record.Name, Type: r.record.Type, Want: r.record.Targets, Got: r.Got, State: r.State,
		}
		if !r.Since.IsZero() && r.State != dnsStatePresent {
			report.Since = r.Since.UTC().Format(time.RFC3339)
		}
		entry.Records = append(entry.Records, report)
	}
	out := DNSStatus{CheckedAt: now.UTC().Format(time.RFC3339)}
	sort.Strings(order)
	for _, name := range order {
		entry := by[name]
		sort.SliceStable(entry.Records, func(i, j int) bool {
			a, b := entry.Records[i], entry.Records[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Type < b.Type
		})
		out.Domains = append(out.Domains, *entry)
	}
	return out
}

// writeDomainStatus writes the status, creating the ConfigMap the first time.
// A failure is logged and the next pass tries again: the lookups are what
// matter, and they have already been reported.
func (v *DNSPublicationVerifier) writeDomainStatus(ctx context.Context, domains []servedDomain, results []dnsRecordStatus, now time.Time) {
	if v.Namespace == "" {
		return
	}
	logger := log.FromContext(ctx).WithName("dns-publication")
	raw, err := json.Marshal(domainStatus(domains, results, now))
	if err != nil {
		logger.Error(err, "cannot encode the DNS status")
		return
	}
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: v.Namespace, Name: DNSStatusConfigMapName}
	switch err := v.Client.Get(ctx, key, cm); {
	case errors.IsNotFound(err):
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: DNSStatusConfigMapName, Namespace: v.Namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": managedByValue},
			},
			Data: map[string]string{DNSStatusKey: string(raw)},
		}
		if err := v.Client.Create(ctx, cm); err != nil {
			logger.Error(err, "cannot write the DNS status")
		}
	case err != nil:
		logger.Error(err, "cannot read the DNS status")
	default:
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[DNSStatusKey] = string(raw)
		if err := v.Client.Update(ctx, cm); err != nil {
			logger.Error(err, "cannot write the DNS status")
		}
	}
}
