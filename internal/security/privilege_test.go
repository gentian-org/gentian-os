/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// profileAsking is a profile requesting one privilege of each kind, plus a
// second egress rule, so that "granted" can be told from "all of them".
func profileAsking() *gentianov1alpha1.ComponentProfile {
	port := intstr.FromInt32(587)
	return &gentianov1alpha1.ComponentProfile{
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					PodSecurity: []gentianov1alpha1.PodSecurityWaiver{{
						Name: "run-as-root", Policy: "gentian-require-non-root",
						Scope: "collabora", Reason: "the converter forks as root",
					}},
					Egress: []gentianov1alpha1.EgressRequest{
						{Name: "smtp-relay", Reason: "sends invitations through the relay",
							Rule: &networkingv1.NetworkPolicyEgressRule{
								Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
							}},
						{Name: "webhooks", Reason: "posts to the customer's endpoint",
							Rule: &networkingv1.NetworkPolicyEgressRule{}},
					},
					ClusterRoles: []gentianov1alpha1.ClusterRoleRequest{{
						Name: "read-nodes", Reason: "renders a capacity view",
					}},
				},
			},
		},
	}
}

func componentGranting(refs ...string) *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	for _, ref := range refs {
		comp.Spec.Privileges = append(comp.Spec.Privileges, gentianov1alpha1.PrivilegeGrant{
			Privilege: ref, Approver: "tom", ApprovedAt: metav1.Now(),
			Reason: "agreed in the security review",
		})
	}
	return comp
}

func TestRequestedPrivilegesAreSortedAndComplete(t *testing.T) {
	got := security.RequestedPrivileges(profileAsking())
	want := []string{"clusterRoles/read-nodes", "egress/smtp-relay", "egress/webhooks", "podSecurity/run-as-root"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requested = %v, want %v", got, want)
	}
	// A profile asking for nothing asks for nothing: no empty slice to make a
	// status field flap between nil and [].
	if got := security.RequestedPrivileges(&gentianov1alpha1.ComponentProfile{}); got != nil {
		t.Fatalf("a profile with no requires asked for %v", got)
	}
}

func TestPendingIsWhatNobodyGranted(t *testing.T) {
	profile := profileAsking()
	comp := componentGranting("egress/smtp-relay", "podSecurity/run-as-root")
	got := security.PendingPrivileges(profile, comp, time.Now())
	want := []string{"clusterRoles/read-nodes", "egress/webhooks"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	// Nothing requested means nothing pending, whatever was granted.
	if got := security.PendingPrivileges(&gentianov1alpha1.ComponentProfile{}, comp, time.Now()); got != nil {
		t.Fatalf("pending with no requests = %v", got)
	}
}

// An expired grant is not a grant. This is the property that makes bounding
// one worth doing: the privilege stops applying by itself and the install
// holds again, rather than running on an approval nobody renewed.
func TestAnExpiredGrantBecomesPendingAgain(t *testing.T) {
	profile := profileAsking()
	comp := componentGranting("egress/smtp-relay")
	yesterday := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	comp.Spec.Privileges[0].ExpiresAt = &yesterday

	pending := security.PendingPrivileges(profile, comp, time.Now())
	found := false
	for _, p := range pending {
		if p == "egress/smtp-relay" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an expired grant still counted; pending = %v", pending)
	}
	if rules := security.GrantedEgressRules(profile, comp, time.Now()); len(rules) != 0 {
		t.Fatalf("an expired grant still opened %d egress rule(s)", len(rules))
	}
	// And it applies right up to the instant it expires, not before.
	if rules := security.GrantedEgressRules(profile, comp, yesterday.Add(-time.Minute)); len(rules) != 1 {
		t.Fatalf("a grant one minute before expiry opened %d rules, want 1", len(rules))
	}
}

// Only what was granted reaches the policy, and in the profile's order.
func TestGrantedEgressRulesAreTheGrantedOnes(t *testing.T) {
	profile := profileAsking()
	rules := security.GrantedEgressRules(profile, componentGranting("egress/webhooks"), time.Now())
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want the one that was granted", len(rules))
	}
	if len(rules[0].Ports) != 0 {
		t.Fatalf("got the smtp-relay rule; the granted one was webhooks")
	}
	both := security.GrantedEgressRules(profile, componentGranting("egress/webhooks", "egress/smtp-relay"), time.Now())
	if len(both) != 2 || len(both[0].Ports) != 1 {
		t.Fatalf("two grants gave %d rules, and the first was not the profile's first", len(both))
	}
	// A grant for something no profile asks for opens nothing.
	if r := security.GrantedEgressRules(profile, componentGranting("egress/invented"), time.Now()); len(r) != 0 {
		t.Fatalf("a grant naming no request opened %d rules", len(r))
	}
}

// Who approves follows from the kind. If this ever returns "tenant" for pod
// security, a tenant administrator can waive a rule that protects the node.
func TestScopeFollowsTheKindNotTheProfile(t *testing.T) {
	if s := security.PrivilegeScope(security.PrivilegeEgress); s != "tenant" {
		t.Fatalf("egress is approved at %q scope, want tenant", s)
	}
	for _, kind := range []string{security.PrivilegePodSecurity, security.PrivilegeClusterRoles} {
		if s := security.PrivilegeScope(kind); s != "cluster" {
			t.Fatalf("%s is approved at %q scope, want cluster", kind, s)
		}
	}
	// An unknown kind is cluster scope: the strictest answer, so a kind added
	// to the API and forgotten here cannot be approved by the cheaper party.
	if s := security.PrivilegeScope("something-new"); s != "cluster" {
		t.Fatalf("an unknown kind is approved at %q scope, want cluster", s)
	}
}

// What a console is told about who answers is PrivilegeScope in other words,
// never a decision of its own: the perimeter approver for egress, break-glass
// for an API extension and a role that writes, the security officer for
// everything else, an unknown kind or role included.
func TestTheApproverIsTheScopeInAConsolesWords(t *testing.T) {
	for ref, want := range map[string]string{
		"egress/smtp":                 security.PrivilegeApproverPerimeter,
		"podSecurity/host-path":       security.PrivilegeApproverSecurityOfficer,
		"clusterRoles/no-such-role":   security.PrivilegeApproverSecurityOfficer,
		"clusterRoles/tenant-exports": security.PrivilegeApproverBreakGlass,
		"apiExtensions/schedules":     security.PrivilegeApproverBreakGlass,
		"something-new/x":             security.PrivilegeApproverSecurityOfficer,
		"not-a-ref":                   security.PrivilegeApproverSecurityOfficer,
	} {
		if got := security.PrivilegeApprover(ref); got != want {
			t.Errorf("%s is answered by %q, want %q", ref, got, want)
		}
	}
}

func TestSplitPrivilegeRefRejectsWhatIsNotARef(t *testing.T) {
	kind, name := security.SplitPrivilegeRef("egress/smtp-relay")
	if kind != "egress" || name != "smtp-relay" {
		t.Fatalf("split = %q, %q", kind, name)
	}
	for _, bad := range []string{"", "egress", "/name", "egress/", "egress"} {
		if k, n := security.SplitPrivilegeRef(bad); k != "" || n != "" {
			t.Fatalf("%q split into %q, %q; want it to match nothing", bad, k, n)
		}
	}
}

func TestPrivilegeReasonIsTheProfilesOwn(t *testing.T) {
	profile := profileAsking()
	if got := security.PrivilegeReason(profile, "podSecurity/run-as-root"); got != "the converter forks as root" {
		t.Fatalf("reason = %q", got)
	}
	if got := security.PrivilegeReason(profile, "egress/webhooks"); got != "posts to the customer's endpoint" {
		t.Fatalf("reason = %q", got)
	}
	if got := security.PrivilegeReason(profile, "egress/nothing"); got != "" {
		t.Fatalf("a ref the profile does not ask for had reason %q", got)
	}
}

// A request by host opens no rule by address, and its hosts are the
// component's only once the request was granted or the cluster's policy
// answers it.
func TestGrantedHostsAreThoseOfAGrantedRequest(t *testing.T) {
	profile := profileAsking()
	profile.Spec.Requires.Privileges.Egress = append(profile.Spec.Requires.Privileges.Egress, gentianov1alpha1.EgressRequest{
		Name: "outside", Reason: "reads the calendars people connected",
		Hosts: []string{"graph.microsoft.com", "login.microsoftonline.com"},
	})
	now := time.Now()
	if got := security.GrantedEgressHostsUnder(profile, componentGranting("egress/smtp-relay"), now, nil); len(got) != 0 {
		t.Errorf("hosts without their grant: %v", got)
	}
	got := security.GrantedEgressHostsUnder(profile, componentGranting("egress/outside"), now, nil)
	if strings.Join(got, ",") != "graph.microsoft.com,login.microsoftonline.com" {
		t.Errorf("granted hosts = %v", got)
	}
	if rules := security.GrantedEgressRulesUnder(profile, componentGranting("egress/outside"), now, nil); len(rules) != 0 {
		t.Errorf("a request by host opened rules by address: %v", rules)
	}
	answered := map[string]struct{}{"egress/outside": {}}
	if got := security.GrantedEgressHostsUnder(profile, componentGranting(), now, answered); len(got) != 2 {
		t.Errorf("hosts the cluster's policy answers = %v", got)
	}
	if got := security.GrantedEgressHostsUnder(nil, nil, now, answered); got != nil {
		t.Errorf("hosts of no profile = %v", got)
	}
}

// A request whose hosts are named at approval is answered only by a grant
// that names hosts, opens those hosts and no others, and is answered by no
// policy and no trusted domain: nothing is known to open before somebody
// names it.
func TestHostsNamedAtApprovalAreTheGrantsOwn(t *testing.T) {
	profile := profileAsking()
	profile.Spec.Requires.Privileges.Egress = append(profile.Spec.Requires.Privileges.Egress, gentianov1alpha1.EgressRequest{
		Name: "backends", Reason: "sends the plans to the ERP its owner runs", HostsAtApproval: true,
	})
	now := time.Now()
	named := componentGranting("egress/backends")
	named.Spec.Privileges[0].Hosts = []string{"erp.example.org", "files.example.org:8443"}
	if got := security.GrantedEgressHostsUnder(profile, named, now, nil); strings.Join(got, ",") != "erp.example.org,files.example.org:8443" {
		t.Errorf("hosts named at approval = %v", got)
	}
	if pending := security.PendingPrivileges(profile, named, now); slices.Contains(pending, "egress/backends") {
		t.Errorf("a grant naming hosts left the request pending: %v", pending)
	}
	if rules := security.GrantedEgressRulesUnder(profile, named, now, nil); len(rules) != 0 {
		t.Errorf("hosts named at approval opened rules by address: %v", rules)
	}

	empty := componentGranting("egress/backends")
	if got := security.GrantedEgressHostsUnder(profile, empty, now, nil); len(got) != 0 {
		t.Errorf("a grant naming no host opened %v", got)
	}
	if pending := security.PendingPrivileges(profile, empty, now); !slices.Contains(pending, "egress/backends") {
		t.Errorf("a grant naming no host answered the request: %v", pending)
	}

	expired := componentGranting("egress/backends")
	expired.Spec.Privileges[0].Hosts = []string{"erp.example.org"}
	past := metav1.NewTime(now.Add(-time.Hour))
	expired.Spec.Privileges[0].ExpiresAt = &past
	if got := security.GrantedEgressHostsUnder(profile, expired, now, nil); len(got) != 0 {
		t.Errorf("an expired grant opened %v", got)
	}

	if _, ok := security.TrustedEgress(profile, []string{"org", "example.org"})["egress/backends"]; ok {
		t.Error("a trusted domain answered a request whose hosts are named at approval")
	}

	// Hosts on the grant of a request that names its own are not opened.
	fixed := profileAsking()
	fixed.Spec.Requires.Privileges.Egress = append(fixed.Spec.Requires.Privileges.Egress, gentianov1alpha1.EgressRequest{
		Name: "outside", Reason: "reads the calendars people connected", Hosts: []string{"graph.microsoft.com"},
	})
	extra := componentGranting("egress/outside")
	extra.Spec.Privileges[0].Hosts = []string{"evil.example.net"}
	if got := security.GrantedEgressHostsUnder(fixed, extra, now, nil); strings.Join(got, ",") != "graph.microsoft.com" {
		t.Errorf("a grant's hosts widened a request that names its own: %v", got)
	}
}

// A request by host is answered by trusted domains when every one of its
// hosts is in one, or under one. A domain covers whole labels, a request
// with one host outside is a person's to answer, and a request by address
// is in no domain.
func TestTrustedDomainsAnswerARequestWhoseHostsAreAllInThem(t *testing.T) {
	profile := profileAsking()
	profile.Spec.Requires.Privileges.Egress = append(profile.Spec.Requires.Privileges.Egress, gentianov1alpha1.EgressRequest{
		Name: "outside", Reason: "reads the calendars people connected",
		Hosts: []string{"graph.microsoft.com", "login.microsoftonline.com:8443"},
	})
	for name, c := range map[string]struct {
		domains  []string
		answered bool
	}{
		"both domains":             {[]string{"microsoft.com", "microsoftonline.com"}, true},
		"the hosts themselves":     {[]string{"graph.microsoft.com", "login.microsoftonline.com"}, true},
		"one host outside":         {[]string{"microsoft.com"}, false},
		"no domain":                {nil, false},
		"a name that only ends so": {[]string{"soft.com", "online.com"}, false},
		"a top-level name":         {[]string{"com"}, true},
		"an empty entry":           {[]string{""}, false},
	} {
		got := security.TrustedEgress(profile, c.domains)
		if _, ok := got["egress/outside"]; ok != c.answered {
			t.Errorf("%s: answered = %v, want %v", name, ok, c.answered)
		}
		for ref := range got {
			if ref != "egress/outside" {
				t.Errorf("%s: a request by address was answered: %s", name, ref)
			}
		}
	}
	for host, want := range map[string]bool{
		"example.org": true, "api.example.org": true, "api.example.org:8443": true,
		"notexample.org": false, "example.org.evil.net": false, "org": false,
	} {
		if got := security.InTrustedDomain(host, []string{"example.org"}); got != want {
			t.Errorf("%s in example.org = %v", host, got)
		}
	}

	// A tenant's own list counts only while the cluster delegated one.
	tenant := &gentianov1alpha1.Tenant{Spec: gentianov1alpha1.TenantSpec{TrustedDomains: &gentianov1alpha1.TenantTrustedDomains{Domains: []string{"example.org"}}}}
	if got := security.TenantTrustedDomains(tenant); got != nil {
		t.Errorf("a tenant's list without the delegation: %v", got)
	}
	tenant.Spec.TrustedDomains.Delegated = true
	if got := security.TenantTrustedDomains(tenant); len(got) != 1 || got[0] != "example.org" {
		t.Errorf("a delegated tenant's list: %v", got)
	}
	if got := security.TenantTrustedDomains(nil); got != nil {
		t.Errorf("no tenant: %v", got)
	}
}
