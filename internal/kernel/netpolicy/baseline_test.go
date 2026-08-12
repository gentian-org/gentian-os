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

package netpolicy_test

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
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
		meta.EnvoyGatewayInstallNamespace,
		meta.KernelNamespace,
		meta.OperatorNamespace,
	} {
		if !got[ns] {
			t.Errorf("baseline policy does not allow ingress from %q (allowed: %v)", ns, got)
		}
	}
}
