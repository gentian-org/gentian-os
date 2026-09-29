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
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Which networks the kernel Postfix may relay for without a credential.
//
// This exists because the alternative — writing the range into a values file —
// is a guess about the cloud provider that is wrong the first time the platform
// moves. Postfix grants relaying to mynetworks, the load balancer proxies
// inbound mail from an address of its own, and whether that address falls
// inside the configured range is a property of the provider's address plan:
// Infomaniak puts nodes on 172.21.0.0/16, AWS and GCP routinely put them inside
// 10.0.0.0/8. A range wide enough to be portable therefore trusts the load
// balancer on some providers, which means trusting every client on the
// internet — an open relay that announces itself only when the sending address
// reaches a blocklist.
//
// A Pod CIDR is a Kubernetes fact rather than a provider fact, so deriving it
// is correct everywhere and needs no per-cluster value. It is also the exact
// set being described: "senders that are Pods in this cluster".
//
// Deriving is the fallback, not the mechanism. Senders that can authenticate
// should, because a credential keeps its meaning when the cluster is rebuilt
// somewhere else — see the SASL settings in the Postfix manifests. This covers
// the workloads that cannot yet.
const postfixMyNetworksKey = "mynetworks"

// loopbackNetworks are always trusted: Postfix's own sendmail(1) submissions
// arrive over the loopback interface, and a list without them stops the
// container generating its own bounces and notifications.
const loopbackNetworks = "127.0.0.0/8,[::1]/128"

// podNetworks returns the cluster's Pod CIDRs as a Postfix mynetworks value.
//
// Empty when no node declares one, which is not an error: several CNIs — Calico
// with its own IPAM is the common case — run their address management outside
// the Node object and leave spec.podCIDR unset. The caller writes nothing then,
// so the chart's configured value stays in force, and such a cluster is the
// "needs a bit of tweaking" case rather than a broken one.
//
// Returning an empty-but-successful result matters more than it looks: writing
// a list that is merely the loopback range would silently stop every in-cluster
// sender relaying, which reads as "mail is broken" long before anyone connects
// it to a CNI that does not populate a field.
func podNetworks(ctx context.Context, nodes *corev1.NodeList) string {
	seen := map[string]bool{}
	out := []string{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		// PodCIDRs is the dual-stack form and supersedes PodCIDR, which is kept
		// in step by the API server for single-stack clusters. Reading the list
		// first means an IPv6 cluster is covered without a second code path.
		cidrs := node.Spec.PodCIDRs
		if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
			cidrs = []string{node.Spec.PodCIDR}
		}
		for _, c := range cidrs {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] {
				continue
			}
			// Parsed rather than trusted. A malformed entry would be passed
			// through to main.cf, and Postfix refuses to start on a mynetworks
			// it cannot parse — taking down inbound mail, which has nothing to
			// do with who may relay.
			if _, _, err := net.ParseCIDR(c); err != nil {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	// Sorted so an unchanged cluster produces an unchanged value: the ConfigMap
	// is compared before it is written, and node ordering from the API server is
	// not stable. Without this every reconcile would rewrite the key and restart
	// Postfix through the reloader annotation.
	sort.Strings(out)
	return loopbackNetworks + "," + strings.Join(out, ",")
}
