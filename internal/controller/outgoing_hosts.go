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
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
	"github.com/gentian-org/gentian-os/internal/egress"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
)

// A component's side of the gateway for outgoing connections.
//
// The component says which hosts outside it was granted
// (status.outgoingHosts); the outgoing reconciler gives each a listener and a
// Service in the edge namespace (outgoing_gateway_reconciler.go). The
// component then reads where its hosts are carried: the Service's address
// goes into its pods' host table (apprender, the host rule), and the
// listener's port into its own network policy. Until a host is carried the
// component waits, since a pod started without the entry would resolve the
// host's real address, which its policy does not open.

// grantedOutgoingHosts are the hosts outside the cluster a component was
// granted, as they are written in its status: each once, in order. signIn
// are the hosts the liaison reaches for the tenant's bridges
// (signInHosts). refusal names a granted host that is not a host outside,
// which the schema cannot tell and which is then carried nowhere.
func grantedOutgoingHosts(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, preApproved map[string]struct{}, signIn []string) (hosts []string, refusal string) {
	seen := map[egress.Host]bool{}
	var parsed []egress.Host
	var refused []string
	for _, s := range append(security.GrantedEgressHostsUnder(profile, comp, time.Now(), preApproved), signIn...) {
		h, err := egress.ParseHost(s)
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		if !seen[h] {
			seen[h] = true
			parsed = append(parsed, h)
		}
	}
	sort.Slice(parsed, func(i, j int) bool {
		if parsed[i].Name != parsed[j].Name {
			return parsed[i].Name < parsed[j].Name
		}
		return parsed[i].Port < parsed[j].Port
	})
	for _, h := range parsed {
		hosts = append(hosts, h.String())
	}
	return hosts, strings.Join(refused, "; ")
}

// outgoingWays is where a component's granted hosts are carried.
type outgoingWays struct {
	// aliases are the entries of the pods' host table.
	aliases []apprender.HostAlias
	// rule opens the fleet's pods on the listeners of the hosts; nil with no
	// host.
	rule *networkingv1.NetworkPolicyEgressRule
	// waiting names the hosts the gateway does not carry yet.
	waiting []string
}

// readOutgoingWays reads, for the hosts in a component's status, the Service
// of each host name on the gateway for outgoing connections: its address,
// and the listener its port for the host forwards to.
func readOutgoingWays(ctx context.Context, c client.Reader, hosts []string) (outgoingWays, error) {
	var out outgoingWays
	if len(hosts) == 0 {
		return out, nil
	}
	edge := layout.Namespace(layout.Edge)
	services := map[string]*corev1.Service{}
	ports := map[egress.Host]int32{}
	var carried []egress.Host
	aliased := map[string]bool{}
	for _, s := range hosts {
		h, err := egress.ParseHost(s)
		if err != nil {
			// Written by this reconciler from hosts it parsed.
			return out, fmt.Errorf("status.outgoingHosts: %w", err)
		}
		svc, read := services[h.Name]
		if !read {
			svc = &corev1.Service{}
			err := c.Get(ctx, types.NamespacedName{Namespace: edge, Name: egress.ServiceName(h.Name)}, svc)
			switch {
			case errors.IsNotFound(err):
				svc = nil
			case err != nil:
				return out, err
			}
			services[h.Name] = svc
		}
		listener := int32(0)
		if svc != nil && svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != corev1.ClusterIPNone {
			for _, p := range svc.Spec.Ports {
				if p.Port == h.Port {
					listener = p.TargetPort.IntVal
				}
			}
		}
		if listener == 0 {
			out.waiting = append(out.waiting, s)
			continue
		}
		ports[h] = listener
		carried = append(carried, h)
		if !aliased[h.Name] {
			aliased[h.Name] = true
			out.aliases = append(out.aliases, apprender.HostAlias{IP: svc.Spec.ClusterIP, Hostname: h.Name})
		}
	}
	out.rule = egress.CallerRule(edge, carried, ports)
	return out, nil
}

// outgoingGatewayPending is what a component says while it waits for a
// granted host to be carried.
func outgoingGatewayPending(waiting []string) string {
	return fmt.Sprintf("waiting for the gateway for outgoing connections to carry %s; "+
		"a cluster without the GatewayClass %s carries none, and the installer's step A-05 applies it",
		strings.Join(waiting, ", "), netpolicy.OutgoingGatewayClassName)
}
