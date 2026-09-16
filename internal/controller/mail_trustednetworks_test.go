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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nodeWith(name, cidr string, cidrs ...string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{PodCIDR: cidr, PodCIDRs: cidrs},
	}
}

// The property that matters is not "returns a list" but "returns the Pod range
// and nothing else". A value that also covers the node network is what made this
// an open relay, so the node addresses are deliberately present in the fixtures.
func TestPodNetworksCoversPodRangeOnly(t *testing.T) {
	nodes := &corev1.NodeList{Items: []corev1.Node{
		nodeWith("a", "10.64.7.0/24"),
		nodeWith("b", "10.64.8.0/24"),
	}}
	got := podNetworks(context.Background(), nodes)
	for _, want := range []string{"10.64.7.0/24", "10.64.8.0/24", "127.0.0.0/8"} {
		if !strings.Contains(got, want) {
			t.Fatalf("podNetworks() = %q, missing %q", got, want)
		}
	}
	// The Infomaniak node and load-balancer network. Trusting it is what let the
	// internet relay, because inbound mail is proxied from it.
	if strings.Contains(got, "172.") || strings.Contains(got, "192.168") {
		t.Fatalf("podNetworks() = %q, must not trust node or load-balancer networks", got)
	}
}

// Node ordering from the API server is not stable, and an unstable value would
// rewrite the ConfigMap on every reconcile — restarting Postfix each time
// through the reloader annotation.
func TestPodNetworksIsStableAndDeduplicated(t *testing.T) {
	forward := podNetworks(context.Background(), &corev1.NodeList{Items: []corev1.Node{
		nodeWith("a", "10.64.7.0/24"),
		nodeWith("b", "10.64.8.0/24"),
		nodeWith("c", "10.64.7.0/24"),
	}})
	reverse := podNetworks(context.Background(), &corev1.NodeList{Items: []corev1.Node{
		nodeWith("c", "10.64.7.0/24"),
		nodeWith("b", "10.64.8.0/24"),
		nodeWith("a", "10.64.7.0/24"),
	}})
	if forward != reverse {
		t.Fatalf("ordering changed the value: %q vs %q", forward, reverse)
	}
	if strings.Count(forward, "10.64.7.0/24") != 1 {
		t.Fatalf("duplicate CIDR not collapsed: %q", forward)
	}
}

// Empty, not loopback-only. Several CNIs run their own IPAM and never populate
// spec.podCIDR; the caller writes no key then, so the chart's configured value
// stays in force. Returning "127.0.0.0/8" instead would override that value with
// one that silently stops every in-cluster sender relaying.
func TestPodNetworksEmptyWhenClusterDeclaresNone(t *testing.T) {
	if got := podNetworks(context.Background(), &corev1.NodeList{Items: []corev1.Node{
		nodeWith("a", ""),
	}}); got != "" {
		t.Fatalf("podNetworks() = %q, want empty so the configured fallback applies", got)
	}
}

// A malformed entry reaches main.cf otherwise, and Postfix refuses to start on a
// mynetworks it cannot parse — taking inbound mail down over a question about
// who may relay.
func TestPodNetworksSkipsUnparseableCIDR(t *testing.T) {
	got := podNetworks(context.Background(), &corev1.NodeList{Items: []corev1.Node{
		nodeWith("a", "not-a-cidr"),
		nodeWith("b", "10.64.8.0/24"),
	}})
	if strings.Contains(got, "not-a-cidr") {
		t.Fatalf("podNetworks() = %q, must not pass through an unparseable CIDR", got)
	}
	if !strings.Contains(got, "10.64.8.0/24") {
		t.Fatalf("podNetworks() = %q, one bad node must not discard the good ones", got)
	}
}

// Dual-stack clusters carry the list, and PodCIDR alone would cover only the
// first family.
func TestPodNetworksReadsDualStackList(t *testing.T) {
	got := podNetworks(context.Background(), &corev1.NodeList{Items: []corev1.Node{
		nodeWith("a", "10.64.8.0/24", "10.64.8.0/24", "fd00::/64"),
	}})
	if !strings.Contains(got, "fd00::/64") {
		t.Fatalf("podNetworks() = %q, missing the IPv6 range", got)
	}
}

// The kernel domain published a DKIM key and nothing else: mail was signed as
// it, no receiver was told which hosts may send as it, and no policy said what
// to do about the ones that are not. It is also the domain the open-relay spam
// forged, so the absence was not theoretical.
func TestMailSPFRecordNamesTheSendingHostNotTheMX(t *testing.T) {
	got := mailSPFRecord("mail-egress.gentian.cloud")
	if got != "v=spf1 a:mail-egress.gentian.cloud -all" {
		t.Fatalf("mailSPFRecord() = %q", got)
	}
	// "mx" would name the inbound load balancer, which never sends, so the
	// record would fail by construction while looking plausible.
	if strings.Contains(got, "mx") {
		t.Fatalf("mailSPFRecord() = %q, must not authorise the MX as a sender", got)
	}
}

// Without a dedicated egress the record is a guess, and a guess must not tell
// receivers to reject.
func TestMailSPFRecordSoftFailsWhenEgressUnknown(t *testing.T) {
	if got := mailSPFRecord(""); !strings.HasSuffix(got, "~all") {
		t.Fatalf("mailSPFRecord(\"\") = %q, want a soft fail", got)
	}
}

// p=none because the reports are what say whether a stricter policy would
// bounce real mail. Publishing reject first is how a domain silences its own
// invites.
func TestMailDMARCRecordReportsBeforeItRejects(t *testing.T) {
	got := mailDMARCRecord("gentian.cloud")
	if !strings.Contains(got, "p=none") {
		t.Fatalf("mailDMARCRecord() = %q, want p=none until reports say otherwise", got)
	}
	if !strings.Contains(got, "rua=mailto:dmarc@gentian.cloud") {
		t.Fatalf("mailDMARCRecord() = %q, missing the reporting address", got)
	}
}

// A deleted tenant must not keep a working login. The keys are enumerated in one
// place precisely so that adding an app to the writer cannot leave the remover
// behind — this asserts that every app the writer knows about is listed.
func TestMailPasswdFileKeysCoverEveryApp(t *testing.T) {
	keys := mailPasswdFileKeys("corp")
	for _, app := range []string{mailAppPasswordApp, mailSubmissionApp, mailAppSubmissionApp} {
		var foundUsers, foundConf bool
		for _, k := range keys {
			if k == app+"-corp.users" {
				foundUsers = true
			}
			if k == app+"-corp.conf" {
				foundConf = true
			}
		}
		if !foundUsers || !foundConf {
			t.Fatalf("mailPasswdFileKeys() = %v, missing %s files", keys, app)
		}
	}
}
