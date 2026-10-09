/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// KernelAccessNetworkPolicy grants egress from an app workload to what its
// profile declares it needs of the platform, and optional profile annotations.
//
// A declared store opens the server that store was provisioned on and the
// port it answers on, and nothing beside it: an app that declared MariaDB
// reaches MariaDB and has no path to PostgreSQL, and the reverse.
func KernelAccessNetworkPolicy(
	tenantName, nsName, appName string,
	profile *gentianov1alpha1.ComponentProfile,
	cfg Config,
) *networkingv1.NetworkPolicy {
	if profile == nil {
		return nil
	}
	targets := kernelEgressTargets(profile, cfg)
	if len(targets) == 0 {
		return nil
	}

	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, len(targets))
	for _, t := range targets {
		egress = append(egress, t.rule())
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelPolicyName(appName),
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyKernel),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

func kernelPolicyName(appName string) string {
	name := fmt.Sprintf("kernel-access-%s", appName)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// egressTarget is one namespace an app may reach and the TCP ports it may
// reach there. No ports means the whole namespace.
type egressTarget struct {
	namespace string
	ports     []int32
}

func (t egressTarget) rule() networkingv1.NetworkPolicyEgressRule {
	rule := namespaceEgress(t.namespace)
	for _, p := range t.ports {
		rule.Ports = append(rule.Ports, tcpPort(p))
	}
	return rule
}

func tcpPort(p int32) networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(p)
	return networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port}
}

// kernelEgressTargets is what an app's profile opens, in a stable order.
//
//   - A database: the server of the engine the profile names, on that
//     engine's port. PostgreSQL and MariaDB are two servers in two
//     namespaces, and naming one opens nothing of the other.
//   - A cache: Redis is shared, in the system tier. Memcached is the
//     tenant's own, in the tenant's namespace, and is opened by the
//     tenant-cache policies, not here.
//   - Object storage: the object store, when the profile asks for a bucket.
//     A storage requirement that asks only for files is fulfilled by another
//     app and opens nothing here.
//   - Mail: the mail namespaces, whole. Where an app's mail goes depends on
//     the cluster's mail mode and on names that resolve to load balancers,
//     and is not narrowed here.
//   - Identity: the edge and the identity provider, whole -- for an app that
//     signs people in itself (OIDC or SAML). An app that declares the
//     sign-in sidecar instead is opened to neither.
//   - Models: the model gateway's namespace on the gateway's port. The
//     gateway's database, its cache and the model servers are in the same
//     namespace on other ports, and are not an app's to reach.
//
// A namespace the profile's annotation names is opened whole, as before, and
// that is the wider of the two when it names one a store also opens.
func kernelEgressTargets(profile *gentianov1alpha1.ComponentProfile, cfg Config) []egressTarget {
	var out []egressTarget
	index := map[string]int{}
	add := func(ns string, ports ...int32) {
		if ns == "" {
			return
		}
		i, seen := index[ns]
		if !seen {
			index[ns] = len(out)
			out = append(out, egressTarget{namespace: ns, ports: ports})
			return
		}
		// Whole wins over a list of ports, whichever came first.
		if len(ports) == 0 || len(out[i].ports) == 0 {
			out[i].ports = nil
			return
		}
		out[i].ports = append(out[i].ports, ports...)
	}

	if kr := profile.Services(); kr != nil {
		// Not for an app whose people are signed in by the platform's
		// sidecar: that app never talks to the identity provider. The
		// sidecar does, and its own policy opens that one path for it
		// (app-default.yaml).
		if id := kr.Identity; id != nil && (id.OIDC != nil || id.SAML != nil || id.Sidecar == nil) {
			add(cfg.ServicesNamespace)
			add(layout.Namespace(layout.Authentication))
		}
		switch provisioner.DatabaseEngineOf(profile) {
		case gentianov1alpha1.DatabaseEnginePostgreSQL:
			add(layout.System("postgresql"), provisioner.PostgresPort)
		case gentianov1alpha1.DatabaseEngineMariaDB:
			add(layout.System("mariadb"), provisioner.MariaDBPort)
		}
		if provisioner.CacheEngineOf(profile) == gentianov1alpha1.CacheEngineRedis {
			add(layout.System("cache"), provisioner.RedisPort)
		}
		if provisioner.MatchS3Profile(profile) {
			add(layout.System("s3"), provisioner.ObjectStoragePort)
		}
		if kr.Mail != nil {
			add(layout.System("mail"))
			add(layout.System("mail-dmz"))
		}
		if kr.LLM != nil {
			add(layout.System("llm"), provisioner.ModelGatewayPort)
		}
		// The rights check: the bouncer's listener for the one question,
		// and no other port of the edge.
		if kr.Rights != nil {
			add(cfg.ServicesNamespace, RightsCheckPort)
		}
		// Vouching: the bouncer's listener for the rights check, which a
		// vouching component is given a key for as well, and the registrar,
		// where a person is linked to the component and where it takes a
		// link away. One port of each namespace.
		if kr.Vouching != nil {
			add(cfg.ServicesNamespace, RightsCheckPort)
			add(layout.Namespace(layout.Control), RegistrarPort)
			// And the realm, which it posts its statements to.
			add(layout.Namespace(layout.Authentication))
		}
	}

	for _, ns := range gentianov1alpha1.ProfileKernelEgressNamespaces(profile) {
		add(ns)
	}
	return out
}
