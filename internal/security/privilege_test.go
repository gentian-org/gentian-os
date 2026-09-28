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

package security_test

import (
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
							Rule: networkingv1.NetworkPolicyEgressRule{
								Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
							}},
						{Name: "webhooks", Reason: "posts to the customer's endpoint",
							Rule: networkingv1.NetworkPolicyEgressRule{}},
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
