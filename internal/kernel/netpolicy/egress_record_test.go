/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/security"
)

// withRule gives profile one egress request by address to cidr.
func withRule(profile *gentianov1alpha1.ComponentProfile, name, cidr string) *networkingv1.NetworkPolicyEgressRule {
	rule := &networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{Name: name, Rule: rule, Reason: "the server this component reaches"}},
	}}
	return rule
}

func digestOf(t *testing.T, rule *networkingv1.NetworkPolicyEgressRule) string {
	t.Helper()
	d, err := security.EgressRuleDigest(rule)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A tenant app's egress policy is built from the rule its grant recorded:
// the same rule opens, another rule opens nothing.
func TestAnAppsEgressPolicyFollowsTheRecordedRule(t *testing.T) {
	t.Parallel()
	app := appProfile()
	approved := withRule(appProfile(), "relay", "203.0.113.10/32")
	withRule(app, "relay", "203.0.113.10/32")
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{"mailer": app})
	in.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "mailer", Privilege: "egress/relay", Approver: "perimeter-approver",
		ApprovedAt: metav1.Now(), Digest: digestOf(t, approved),
	}}
	got := byName(netpolicy.BuildDesired(in))
	if np := got["app-egress-mailer"]; np == nil || np.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.10/32" {
		t.Fatalf("the recorded rule did not open: %+v", np)
	}

	withRule(app, "relay", "0.0.0.0/0")
	got = byName(netpolicy.BuildDesired(in))
	if np, ok := got["app-egress-mailer"]; ok {
		t.Fatalf("a rule other than the recorded one opened: %+v", np.Spec.Egress)
	}
	if _, ok := netpolicy.ManagedPolicyNames(in)["app-egress-mailer"]; ok {
		t.Error("a policy that is not built is still listed as kept")
	}
}

// An agent bridge's callers reach what the bridge's grant recorded, and not
// a rule the bridge's profile changed to.
func TestABridgesCallersFollowTheRecordedRule(t *testing.T) {
	t.Parallel()
	tools := agentBridgeProfile("", true)
	approved := withRule(agentBridgeProfile("", true), "tool-server", "203.0.113.10/32")
	withRule(tools, "tool-server", "203.0.113.10/32")
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{"web-tools": tools, "chaperone": chaperoneProfile()})
	in.BridgeGrants = []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "web-tools", Privilege: "egress/tool-server", Approver: "perimeter-approver",
		ApprovedAt: metav1.Now(), Digest: digestOf(t, approved),
	}}
	got := byName(netpolicy.BuildDesired(in))
	if callers := got["agent-bridge-callers-chaperone"]; callers == nil || callers.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.10/32" {
		t.Fatalf("the recorded rule did not reach the callers: %+v", callers)
	}

	withRule(tools, "tool-server", "198.51.100.0/24")
	got = byName(netpolicy.BuildDesired(in))
	if callers, ok := got["agent-bridge-callers-chaperone"]; ok {
		t.Fatalf("a rule other than the recorded one reached the callers: %+v", callers.Spec.Egress)
	}
}
