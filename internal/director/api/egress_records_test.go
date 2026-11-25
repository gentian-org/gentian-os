/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/security"
)

// egressAsks are the egress requests the fixture's nextcloud states in
// these tests: one by address, two by host, and one whose hosts are named at
// approval.
const egressAsks = `  requires:
    privileges:
      egress:
        - name: smtp-relay
          reason: sends invitations through the relay
          rule:
            ports:
              - protocol: TCP
                port: 587
        - name: something
          reason: reads the calendars people connected
          hosts: ["login.microsoftonline.com:443", "graph.microsoft.com"]
        - name: other-way
          reason: fetches the previews of shared links
          hosts: ["previews.example.org"]
        - name: backends
          reason: sends the plans to the ERP its owner runs
          hostsAtApproval: true
`

// startEgressAsking is the harness whose nextcloud states egressAsks, with
// seed's files committed beside it.
func startEgressAsking(t *testing.T, seed map[string]string) *harness {
	t.Helper()
	files := map[string]string{dt.CataloguePath("nextcloud.yaml"): dt.PublishingProfileYAML("nextcloud", "shares", false) + egressAsks}
	for path, body := range seed {
		files[path] = body
	}
	return startSeeded(t, nil, func(remote string) { dt.Commit(t, remote, files) })
}

// smtpRelayDigest is the digest of smtp-relay's rule.
func smtpRelayDigest(t *testing.T) string {
	t.Helper()
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(587)
	d, err := security.EgressRuleDigest(&networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{{Port: &port, Protocol: &tcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// An egress approval records what it answers, read from the profile the
// install is at: the digest of a rule, a request's hosts once and sorted
// (hosts in the body do not change them), or for a request whose hosts are
// named at approval the hosts named. A request the install does not state is
// not approved.
func TestAnEgressApprovalRecordsWhatItAnswers(t *testing.T) {
	h := startEgressAsking(t, nil)
	tom := h.token(t, "tenant-demo", "tom")
	base := "/v1/tenants/demo/privileges/nextcloud/egress/"

	if code, out := h.do(t, "PUT", base+"smtp-relay", tom, `{}`); code != http.StatusAccepted {
		t.Fatalf("smtp-relay = %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", base+"something", tom, `{"hosts":["evil.example.net"]}`); code != http.StatusAccepted {
		t.Fatalf("something = %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", base+"backends", tom, `{"hosts":["erp.example.org"]}`); code != http.StatusAccepted {
		t.Fatalf("backends = %d %v", code, out)
	}
	got := h.remoteFile(t, "demo", "privileges.yaml")
	for _, want := range []string{
		"privilege: egress/smtp-relay\n      approver: tom\n",
		"digest: " + smtpRelayDigest(t) + "\n",
		"privilege: egress/something\n",
		"      hosts:\n        - \"graph.microsoft.com\"\n        - \"login.microsoftonline.com\"\n",
		"      hosts:\n        - \"erp.example.org\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("privileges.yaml lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "evil.example.net") {
		t.Errorf("hosts in the body were recorded for a request that names its own:\n%s", got)
	}

	before := h.tip(t)
	if code, _ := h.do(t, "PUT", base+"invented", tom, `{}`); code != http.StatusConflict {
		t.Errorf("a request the install does not state = %d, want 409", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/privileges/wiki/egress/smtp-relay", tom, `{}`); code != http.StatusConflict {
		t.Errorf("an install the tenant does not have = %d, want 409", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused approval committed anyway")
	}
}

// legacyGrants are grants written before grants recorded what they
// answered: none records hosts or a digest.
const legacyGrants = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  privileges:
    - install: nextcloud
      privilege: egress/backends
      approver: tom
      approvedAt: 2026-01-02T03:04:05Z
    - install: nextcloud
      privilege: egress/other-way
      approver: tom
      approvedAt: 2026-01-02T03:04:05Z
      hosts:
        - "previews.example.org"
    - install: nextcloud
      privilege: egress/smtp-relay
      approver: tom
      approvedAt: 2026-01-02T03:04:05Z
      reason: "agreed in the review"
    - install: nextcloud
      privilege: egress/something
      approver: ann
      approvedAt: 2026-01-02T03:04:05Z
    - install: nextcloud
      privilege: egress/gone
      approver: tom
      approvedAt: 2026-01-02T03:04:05Z
    - install: nextcloud
      privilege: podSecurity/run-as-root
      approver: sam
      approvedAt: 2026-01-02T03:04:05Z
    - install: wiki
      privilege: egress/smtp-relay
      approver: tom
      approvedAt: 2026-01-02T03:04:05Z
`

// The director's pass records what each egress grant that records nothing
// answered: a rule's digest and a request's hosts, from the profile the
// install is at, in one commit per tenant. It leaves alone a request whose
// hosts are named at approval, one the profile no longer states, an install
// the tenant does not have, and any other kind. A second pass writes nothing.
func TestTheRecordPassWritesOnceAndOnlyOnce(t *testing.T) {
	h := startEgressAsking(t, map[string]string{
		strings.TrimSuffix(dt.TenantPath("demo"), "tenant.yaml") + "privileges.yaml": legacyGrants,
	})
	before := h.tip(t)
	if n := h.server().RecordEgressAnswers(context.Background()); n != 2 {
		t.Fatalf("recorded %d grants, want 2", n)
	}
	got := h.remoteFile(t, "demo", "privileges.yaml")
	for _, want := range []string{
		"privilege: egress/smtp-relay\n      approver: tom\n      approvedAt: 2026-01-02T03:04:05Z\n      reason: \"agreed in the review\"\n      digest: " + smtpRelayDigest(t) + "\n",
		"privilege: egress/something\n      approver: ann\n      approvedAt: 2026-01-02T03:04:05Z\n      hosts:\n        - \"graph.microsoft.com\"\n        - \"login.microsoftonline.com\"\n",
		"privilege: egress/backends\n      approver: tom\n      approvedAt: 2026-01-02T03:04:05Z\n    - install",
		"privilege: egress/gone\n      approver: tom\n      approvedAt: 2026-01-02T03:04:05Z\n    - install",
		"install: wiki\n      privilege: egress/smtp-relay\n      approver: tom\n      approvedAt: 2026-01-02T03:04:05Z\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("privileges.yaml lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "digest:") != 1 {
		t.Errorf("a grant other than smtp-relay's was given a digest:\n%s", got)
	}
	if n := dt.Git(t, "", "--git-dir", h.remote, "rev-list", "--count", before+"..main"); strings.TrimSpace(n) != "1" {
		t.Errorf("the pass made %s commits, want 1", strings.TrimSpace(n))
	}
	if trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(trailer, "platform:egress-records") {
		t.Errorf("the commit does not name the pass:\n%s", trailer)
	}

	after := h.tip(t)
	if n := h.server().RecordEgressAnswers(context.Background()); n != 0 {
		t.Errorf("a second pass recorded %d grants", n)
	}
	if h.tip(t) != after {
		t.Error("a second pass committed")
	}
}
