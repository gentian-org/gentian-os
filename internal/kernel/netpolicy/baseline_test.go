/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	networkingv1 "k8s.io/api/networking/v1"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The operator provisions into running tenant apps over their own admin APIs
// (ComponentProfile.spec.provisioning.privilegedRole). It runs in OperatorNamespace,
// so dropping that peer silently breaks every such provisioner with a
// connection timeout rather than a clear error.
func TestBaselineNetworkPolicy_AllowsKernelAndOperatorIngress(t *testing.T) {
	t.Parallel()

	policy := netpolicy.BaselineNetworkPolicy("demo", "tenant-demo", netpolicy.DefaultConfig(), nil)

	got := map[string]bool{}
	for _, rule := range policy.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil {
				continue
			}
			got[peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]] = true
		}
	}

	for _, ns := range []string{
		layout.Namespace(layout.Edge),
		layout.Namespace(layout.Authentication),
		meta.OperatorNamespace,
	} {
		if !got[ns] {
			t.Errorf("baseline policy does not allow ingress from %q (allowed: %v)", ns, got)
		}
	}
}

// From the edge namespace a tenant's pods take connections from the Envoy
// proxies alone, where the kernel's network rules are on. The namespace also
// holds cert-manager, external-dns and the bouncer, and an app trusts the
// identity headers of whatever reaches it from there. Off, the whole
// namespace is admitted, as it was before the rules existed.
func TestBaselineNetworkPolicy_AdmitsOnlyTheProxiesOfTheEdge(t *testing.T) {
	t.Parallel()

	edgePeer := func(narrow bool) *networkingv1.NetworkPolicyPeer {
		cfg := netpolicy.DefaultConfig()
		cfg.NarrowEdge = narrow
		policy := netpolicy.BaselineNetworkPolicy("demo", "tenant-demo", cfg, nil)
		for _, rule := range policy.Spec.Ingress {
			for i := range rule.From {
				if sel := rule.From[i].NamespaceSelector; sel != nil &&
					sel.MatchLabels["kubernetes.io/metadata.name"] == layout.Namespace(layout.Edge) {
					return &rule.From[i]
				}
			}
		}
		t.Fatal("the baseline admits nothing from the edge namespace")
		return nil
	}
	if peer := edgePeer(true); peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "envoy" {
		t.Errorf("the edge namespace is admitted with pod selector %v, want the Envoy proxies alone", peer.PodSelector)
	}
	if peer := edgePeer(false); peer.PodSelector != nil {
		t.Errorf("with the rules off the edge namespace is admitted with pod selector %v, want the whole namespace", peer.PodSelector)
	}
}
