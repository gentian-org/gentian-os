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

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The operator provisions into running tenant apps over their own admin APIs
// (AppProfile.spec.provisioning.privilegedRole). It runs in OperatorNamespace,
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
