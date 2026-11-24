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
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// recordingHosts is an outside bridge's component whose grant of
// egress/tools records hosts.
func recordingHosts(name string, hosts ...string) *gentianov1alpha1.Component {
	c := signInComponent("tenant-acme", name, name, "egress/tools")
	c.Spec.Privileges[0].Hosts = hosts
	return c
}

// The caller of the tenant's agent bridges reaches an outside bridge's
// hosts as its grant recorded them: a bridge whose profile now names a host
// its grant did not answer waits, and the caller reaches none of its hosts.
func TestTheCallerReachesWhatAnOutsideBridgesGrantRecorded(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	chaperone := signInComponent("tenant-acme", "chaperone", "chaperone")
	widened := outsideAgentBridge("widened", "mcp.widened.example", false)
	widened.Spec.Requires.Privileges.Egress[0].Hosts = append(widened.Spec.Requires.Privileges.Egress[0].Hosts, "exfil.example.net")
	c := signInClient(t,
		vouchingCaller("chaperone"),
		outsideAgentBridge("plaud", "mcp.plaud.ai", false),
		widened,
		chaperone,
		recordingHosts("plaud", "mcp.plaud.ai"),
		recordingHosts("widened", "mcp.widened.example"),
	)
	got, err := bridgeHosts(context.Background(), c, vouchingCaller("chaperone"), chaperone, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"mcp.plaud.ai"}) {
		t.Fatalf("the caller reaches %v, want only mcp.plaud.ai", got)
	}
}

// The same approval with its record filled in reaches the Component in
// place, in one write: nothing is removed first, so the grant never lapses.
// A new approval (another approvedAt) for other terms replaces the stored one;
// for the same terms it is kept as stored.
func TestARecordedAnswerReachesTheComponentInPlace(t *testing.T) {
	at := metav1.Now()
	later := metav1.NewTime(at.Add(time.Hour))
	stored := []gentianov1alpha1.PrivilegeGrant{
		{Privilege: "egress/outside", Approver: "tom", ApprovedAt: at},
		{Privilege: "egress/relay", Approver: "tom", ApprovedAt: at},
		{Privilege: "egress/again", Approver: "tom", ApprovedAt: at, Hosts: []string{"a.example.org"}},
		{Privilege: "egress/same", Approver: "tom", ApprovedAt: at, Hosts: []string{"a.example.org", "b.example.org"}},
	}
	desired := []gentianov1alpha1.PrivilegeGrant{
		{Privilege: "egress/outside", Approver: "tom", ApprovedAt: at, Hosts: []string{"graph.microsoft.com"}},
		{Privilege: "egress/relay", Approver: "tom", ApprovedAt: at, Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		{Privilege: "egress/again", Approver: "ann", ApprovedAt: later, Hosts: []string{"c.example.org"}},
		{Privilege: "egress/same", Approver: "ann", ApprovedAt: later, Hosts: []string{"b.example.org", "a.example.org"}},
	}
	if got := replacedGrants(stored, desired); !slices.Equal(got, []string{"egress/again"}) {
		t.Fatalf("replaced = %v, want only egress/again (a new approval for other terms)", got)
	}
	kept := keepStoredGrants(stored, desired)
	byName := map[string]gentianov1alpha1.PrivilegeGrant{}
	for _, g := range kept {
		byName[g.Privilege] = g
	}
	if h := byName["egress/outside"].Hosts; !slices.Equal(h, []string{"graph.microsoft.com"}) {
		t.Fatalf("the filled-in hosts did not reach the Component in place: %v", h)
	}
	if byName["egress/relay"].Digest == "" {
		t.Fatal("the filled-in digest did not reach the Component in place")
	}
	if g := byName["egress/same"]; g.Approver != "tom" {
		t.Fatalf("the same terms approved again replaced the stored approval: %+v", g)
	}
}
