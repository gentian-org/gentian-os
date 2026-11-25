/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// profileWithEgress is a profile with one egress request.
func profileWithEgress(e gentianov1alpha1.EgressRequest) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Requires: &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
			Egress: []gentianov1alpha1.EgressRequest{e},
		}},
	}}
}

// byHost is a request by host for hosts.
func byHost(hosts ...string) gentianov1alpha1.EgressRequest {
	return gentianov1alpha1.EgressRequest{Name: "outside", Reason: "reads the calendars people connected", Hosts: hosts}
}

// byAddress is a request by address for TCP port.
func byAddress(port int32) gentianov1alpha1.EgressRequest {
	p := intstr.FromInt32(port)
	tcp := corev1.ProtocolTCP
	return gentianov1alpha1.EgressRequest{Name: "outside", Reason: "sends invitations through the relay",
		Rule: &networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p}}}}
}

// grantRecording is a component with a grant of egress/outside that records
// hosts and digest.
func grantRecording(hosts []string, digest string) *gentianov1alpha1.Component {
	comp := componentGranting("egress/outside")
	comp.Spec.Privileges[0].Hosts = hosts
	comp.Spec.Privileges[0].Digest = digest
	return comp
}

func ruleDigest(t *testing.T, e gentianov1alpha1.EgressRequest) string {
	t.Helper()
	d, err := security.EgressRuleDigest(e.Rule)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The digest of a rule is sha256 over its JSON with sorted keys: the same
// rule written in another order is the same digest, another rule is not.
func TestARulesDigestIsStableUnderFieldOrder(t *testing.T) {
	var a, b networkingv1.NetworkPolicyEgressRule
	if err := json.Unmarshal([]byte(`{"to":[{"ipBlock":{"cidr":"192.0.2.0/24","except":["192.0.2.8/29"]}}],"ports":[{"protocol":"TCP","port":587}]}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"ports":[{"port":587,"protocol":"TCP"}],"to":[{"ipBlock":{"except":["192.0.2.8/29"],"cidr":"192.0.2.0/24"}}]}`), &b); err != nil {
		t.Fatal(err)
	}
	da, err := security.EgressRuleDigest(&a)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := security.EgressRuleDigest(&b)
	if da != db {
		t.Errorf("the same rule in another order: %s and %s", da, db)
	}
	if !strings.HasPrefix(da, "sha256:") || len(da) != len("sha256:")+64 {
		t.Errorf("digest = %q, want sha256:<64 hex>", da)
	}
	other := byAddress(25)
	if d := ruleDigest(t, other); d == da {
		t.Error("another rule had the same digest")
	}
}

// A grant that recorded a request's hosts opens them; a host the profile
// adds later is not opened and the request waits, while the recorded hosts
// it still names stay open; a host the profile drops closes.
func TestAHostsGrantOpensWhatItRecorded(t *testing.T) {
	now := time.Now()
	comp := grantRecording([]string{"graph.microsoft.com", "login.microsoftonline.com"}, "")

	same := profileWithEgress(byHost("graph.microsoft.com", "login.microsoftonline.com:443"))
	if got := security.GrantedEgressHostsUnder(same, comp, now, nil); strings.Join(got, ",") != "graph.microsoft.com,login.microsoftonline.com:443" {
		t.Errorf("recorded hosts opened %v", got)
	}
	if pending := security.PendingPrivileges(same, comp, now); len(pending) != 0 {
		t.Errorf("the request as answered waits: %v", pending)
	}

	added := profileWithEgress(byHost("graph.microsoft.com", "login.microsoftonline.com", "exfil.example.net"))
	if got := security.GrantedEgressHostsUnder(added, comp, now, nil); strings.Join(got, ",") != "graph.microsoft.com,login.microsoftonline.com" {
		t.Errorf("with a host added, opened %v; want the recorded hosts only", got)
	}
	if pending := security.PendingPrivileges(added, comp, now); !slices.Contains(pending, "egress/outside") {
		t.Errorf("a request with a host added does not wait: %v", pending)
	}

	dropped := profileWithEgress(byHost("graph.microsoft.com"))
	if got := security.GrantedEgressHostsUnder(dropped, comp, now, nil); strings.Join(got, ",") != "graph.microsoft.com" {
		t.Errorf("with a host dropped, opened %v", got)
	}
	if pending := security.PendingPrivileges(dropped, comp, now); len(pending) != 0 {
		t.Errorf("a narrower request waits: %v", pending)
	}
}

// A grant that recorded a rule's digest opens the rule while it is the
// same; a changed rule waits and opens nothing, and so does a request that
// changed its form.
func TestARuleGrantOpensTheRuleItRecorded(t *testing.T) {
	now := time.Now()
	relay := byAddress(587)
	comp := grantRecording(nil, ruleDigest(t, relay))

	if rules := security.GrantedEgressRulesUnder(profileWithEgress(relay), comp, now, nil); len(rules) != 1 {
		t.Errorf("a matching digest opened %d rules, want 1", len(rules))
	}
	if pending := security.PendingPrivileges(profileWithEgress(relay), comp, now); len(pending) != 0 {
		t.Errorf("the rule as answered waits: %v", pending)
	}

	changed := profileWithEgress(byAddress(25))
	if rules := security.GrantedEgressRulesUnder(changed, comp, now, nil); len(rules) != 0 {
		t.Errorf("a changed rule opened %v", rules)
	}
	if pending := security.PendingPrivileges(changed, comp, now); !slices.Contains(pending, "egress/outside") {
		t.Errorf("a changed rule does not wait: %v", pending)
	}

	// From address to host: the digest answers no hosts.
	toHosts := profileWithEgress(byHost("graph.microsoft.com"))
	if got := security.GrantedEgressHostsUnder(toHosts, comp, now, nil); len(got) != 0 {
		t.Errorf("a rule's grant opened hosts: %v", got)
	}
	if pending := security.PendingPrivileges(toHosts, comp, now); !slices.Contains(pending, "egress/outside") {
		t.Errorf("a request now by host does not wait: %v", pending)
	}

	// From host to address: recorded hosts answer no rule.
	hostGrant := grantRecording([]string{"graph.microsoft.com"}, "")
	if rules := security.GrantedEgressRulesUnder(profileWithEgress(relay), hostGrant, now, nil); len(rules) != 0 {
		t.Errorf("a hosts grant opened a rule: %v", rules)
	}
	if pending := security.PendingPrivileges(profileWithEgress(relay), hostGrant, now); !slices.Contains(pending, "egress/outside") {
		t.Errorf("a request now by address does not wait: %v", pending)
	}

	// From address to hosts named at approval: a digest names no host.
	atApproval := profileWithEgress(gentianov1alpha1.EgressRequest{Name: "outside", Reason: "sends the plans to the ERP", HostsAtApproval: true})
	if pending := security.PendingPrivileges(atApproval, comp, now); !slices.Contains(pending, "egress/outside") {
		t.Errorf("a request now named at approval does not wait: %v", pending)
	}
}

// A grant that records nothing answers the request as it is until the
// director records it.
func TestAGrantWithoutARecordAnswersTheRequestAsItIs(t *testing.T) {
	now := time.Now()
	comp := componentGranting("egress/outside")
	hosts := profileWithEgress(byHost("graph.microsoft.com", "exfil.example.net"))
	if got := security.GrantedEgressHostsUnder(hosts, comp, now, nil); len(got) != 2 {
		t.Errorf("hosts = %v", got)
	}
	rule := profileWithEgress(byAddress(25))
	if rules := security.GrantedEgressRulesUnder(rule, comp, now, nil); len(rules) != 1 {
		t.Errorf("rules = %v", rules)
	}
	if pending := security.PendingPrivileges(rule, comp, now); len(pending) != 0 {
		t.Errorf("pending = %v", pending)
	}
}

// What the cluster's policy answers -- a trusted domain, a profile the
// platform ships -- is the request as it is: no grant's record is read.
func TestPolicyAnswersTheRequestAsItIs(t *testing.T) {
	now := time.Now()
	stale := grantRecording([]string{"graph.microsoft.com"}, "")
	profile := profileWithEgress(byHost("graph.microsoft.com", "login.microsoftonline.com"))
	answered := security.TrustedEgress(profile, []string{"microsoft.com", "microsoftonline.com"})
	if got := security.GrantedEgressHostsUnder(profile, stale, now, answered); len(got) != 2 {
		t.Errorf("trusted hosts = %v", got)
	}
	if pending := security.PendingPrivilegesUnder(profile, stale, now, answered); len(pending) != 0 {
		t.Errorf("pending under trusted domains = %v", pending)
	}
	shipped := map[string]struct{}{"egress/outside": {}}
	rule := profileWithEgress(byAddress(25))
	if rules := security.GrantedEgressRulesUnder(rule, grantRecording(nil, ruleDigest(t, byAddress(587))), now, shipped); len(rules) != 1 {
		t.Errorf("a shipped profile's rule = %v", rules)
	}
}

// The tenant's netpolicy and an agent bridge's callers read the same answer
// from the Tenant's grants (GrantedEgress over TenantGrants).
func TestTenantGrantsAnswerAsTheComponentsDo(t *testing.T) {
	now := time.Now()
	relay := byAddress(587)
	tenant := &gentianov1alpha1.Tenant{Spec: gentianov1alpha1.TenantSpec{Privileges: []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "relay", Privilege: "egress/outside", Approver: "tom", ApprovedAt: metav1.Now(), Digest: ruleDigest(t, relay),
	}}}}
	grants := security.TenantGrants(tenant, "relay")
	if got := security.GrantedEgress(profileWithEgress(relay), grants, now, nil); len(got.Rules) != 1 {
		t.Errorf("the recorded rule = %v", got.Rules)
	}
	if got := security.GrantedEgress(profileWithEgress(byAddress(25)), grants, now, nil); len(got.Rules) != 0 {
		t.Errorf("a changed rule opened %v", got.Rules)
	}
}

// What a request records when a person answers it: hosts once, sorted and
// as a grant writes them; or the rule's digest; nothing for hosts named at
// approval.
func TestEgressRecordIsTheRequestAsItIs(t *testing.T) {
	e := byHost("login.microsoftonline.com:443", "graph.microsoft.com", "graph.microsoft.com")
	hosts, digest, err := security.EgressRecord(&e)
	if err != nil || digest != "" || strings.Join(hosts, ",") != "graph.microsoft.com,login.microsoftonline.com" {
		t.Errorf("hosts record = %v %q %v", hosts, digest, err)
	}
	r := byAddress(587)
	if hosts, digest, _ := security.EgressRecord(&r); hosts != nil || digest != ruleDigest(t, r) {
		t.Errorf("rule record = %v %q", hosts, digest)
	}
	a := gentianov1alpha1.EgressRequest{Name: "x", HostsAtApproval: true}
	if hosts, digest, _ := security.EgressRecord(&a); hosts != nil || digest != "" {
		t.Errorf("named at approval = %v %q", hosts, digest)
	}
}
