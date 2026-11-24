/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What an egress grant opens.
//
// A grant of egress/<name> records what the approver answered: the hosts of
// a request by host (Hosts), the digest of a request by address (Digest,
// EgressRuleDigest), or the hosts the approver named for a request whose
// hosts are named at approval. What opens is what the request asks now and
// the grant answered: the hosts of both, or the rule while its digest is the
// one recorded. A request that asks for more than was answered -- another
// host, another rule, another form -- waits for a new answer, and what was
// answered stays open meanwhile.
//
// A grant without a record answers the request as it is until the director
// records it (the director's fill-in pass, internal/director/api/
// egress_records.go). The cluster's policy -- trusted domains, a profile the
// platform ships -- answers a request as it is, every time: no person's
// answer is recorded for it.
//
// Every reader that turns grants into egress -- the component's network
// policy and its outgoing hosts, the tenant's netpolicy, an agent bridge's
// callers, the runs of an action, the pending list -- asks GrantedEgress or
// LiveGrants, so that all of them reach the same answer.

// EgressRuleDigest is what a grant of a request by address records: the
// sha256 of the rule written as JSON with every object's keys sorted, so
// that the same rule is one digest however it was written.
func EgressRuleDigest(rule *networkingv1.NetworkPolicyEgressRule) (string, error) {
	if rule == nil {
		return "", nil
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// encoding/json writes a map's keys sorted.
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CanonicalHost is a host as a grant records it: lower case, with its port
// only where it is not 443 -- what egress.ParseHost(h).String() writes for a
// host outside the cluster.
func CanonicalHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ":443")
}

// EgressRecord is what a grant of the request e records when a person
// answers it as it is now: its hosts, each once and sorted, or the digest of
// its rule. A request whose hosts are named at approval records the hosts the
// approver names, which are not the request's; it yields nothing here.
func EgressRecord(e *gentianov1alpha1.EgressRequest) (hosts []string, digest string, err error) {
	switch {
	case e == nil || e.HostsAtApproval:
		return nil, "", nil
	case e.Rule != nil:
		d, err := EgressRuleDigest(e.Rule)
		return nil, d, err
	}
	seen := map[string]bool{}
	for _, h := range e.Hosts {
		c := CanonicalHost(h)
		if !seen[c] {
			seen[c] = true
			hosts = append(hosts, c)
		}
	}
	sort.Strings(hosts)
	return hosts, "", nil
}

// egressAnswer is what one live grant answers of one egress request.
type egressAnswer struct {
	// full says the request as it is now is answered in full.
	full bool
	// hosts are the hosts it opens, in the request's order.
	hosts []string
	// rule is the rule it opens.
	rule *networkingv1.NetworkPolicyEgressRule
}

// answerEgress is what the grant g answers of the request e now. A nil g
// answers nothing.
func answerEgress(e *gentianov1alpha1.EgressRequest, g *gentianov1alpha1.PrivilegeGrant) egressAnswer {
	if g == nil {
		return egressAnswer{}
	}
	switch {
	case e.HostsAtApproval:
		// The hosts the approver named, and no others. A digest is the
		// record of a request by address: another form, another answer.
		if g.Digest != "" || len(g.Hosts) == 0 {
			return egressAnswer{}
		}
		return egressAnswer{full: true, hosts: g.Hosts}
	case e.Rule != nil:
		if len(g.Hosts) > 0 {
			return egressAnswer{}
		}
		if g.Digest == "" {
			// A grant without a record answers the request as it is
			// until the director records it.
			return egressAnswer{full: true, rule: e.Rule}
		}
		if want, err := EgressRuleDigest(e.Rule); err != nil || want != g.Digest {
			return egressAnswer{}
		}
		return egressAnswer{full: true, rule: e.Rule}
	}
	if g.Digest != "" {
		return egressAnswer{}
	}
	if len(g.Hosts) == 0 {
		// A grant without a record answers the request as it is until
		// the director records it.
		return egressAnswer{full: true, hosts: e.Hosts}
	}
	recorded := map[string]bool{}
	for _, h := range g.Hosts {
		recorded[CanonicalHost(h)] = true
	}
	out := egressAnswer{full: true}
	for _, h := range e.Hosts {
		if recorded[CanonicalHost(h)] {
			out.hosts = append(out.hosts, h)
		} else {
			out.full = false
		}
	}
	return out
}

// liveGrant is the live grant of ref among grants, nil when there is none.
func liveGrant(grants []gentianov1alpha1.PrivilegeGrant, ref string, now time.Time) *gentianov1alpha1.PrivilegeGrant {
	for i := range grants {
		g := &grants[i]
		if g.Privilege != ref || (g.ExpiresAt != nil && !now.Before(g.ExpiresAt.Time)) {
			continue
		}
		return g
	}
	return nil
}

// EgressGrants is what grants and the cluster's policy open of a profile's
// egress requests.
type EgressGrants struct {
	// Rules are the rules by address opened, in the profile's order.
	Rules []networkingv1.NetworkPolicyEgressRule
	// Hosts are the hosts outside the cluster opened: those of requests by
	// host in the profile's order, then those named at approval.
	Hosts []string
	// Answered are the egress requests answered in full, as egress/<name>.
	// A request not among them waits for an answer.
	Answered map[string]struct{}
}

// GrantedEgress is the one answer to what a profile's egress requests open
// under grants and the requests the cluster's policy answers (policy, as
// egress/<name>). Every caller that opens network for a grant asks this.
func GrantedEgress(profile *gentianov1alpha1.ComponentProfile, grants []gentianov1alpha1.PrivilegeGrant, now time.Time, policy map[string]struct{}) EgressGrants {
	out := EgressGrants{Answered: map[string]struct{}{}}
	p := profile.Privileges()
	if p == nil {
		return out
	}
	var atApproval []string
	for i := range p.Egress {
		e := &p.Egress[i]
		ref := PrivilegeRef(PrivilegeEgress, e.Name)
		_, byPolicy := policy[ref]
		var a egressAnswer
		if byPolicy && !e.HostsAtApproval {
			// The cluster's policy answers the request as it is.
			a = egressAnswer{full: true, hosts: e.Hosts, rule: e.Rule}
		} else {
			a = answerEgress(e, liveGrant(grants, ref, now))
			a.full = a.full || byPolicy
		}
		if a.full {
			out.Answered[ref] = struct{}{}
		}
		if a.rule != nil {
			out.Rules = append(out.Rules, *a.rule)
		}
		if e.HostsAtApproval {
			atApproval = append(atApproval, a.hosts...)
		} else {
			out.Hosts = append(out.Hosts, a.hosts...)
		}
	}
	out.Hosts = append(out.Hosts, atApproval...)
	return out
}

// sameHosts says whether two host lists name the same hosts.
func sameHosts(a, b []string) bool {
	ca := make([]string, 0, len(a))
	for _, h := range a {
		ca = append(ca, CanonicalHost(h))
	}
	cb := make([]string, 0, len(b))
	for _, h := range b {
		cb = append(cb, CanonicalHost(h))
	}
	sort.Strings(ca)
	sort.Strings(cb)
	return slices.Equal(slices.Compact(ca), slices.Compact(cb))
}

// SameRecord says whether two grants of one privilege record the same
// answer: the same digest and the same hosts.
func SameRecord(a, b *gentianov1alpha1.PrivilegeGrant) bool {
	return a.Digest == b.Digest && sameHosts(a.Hosts, b.Hosts)
}
