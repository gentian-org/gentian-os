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

package netpolicy

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/meta"
)

// The policy must be scoped to export pods and grant exactly the two
// namespaces a capture needs. An empty pod selector here would hand every
// tenant workload egress to the infra namespace — the opposite of the MAC
// floor the baseline establishes.
func TestExportJobNetworkPolicyIsScopedToExportPods(t *testing.T) {
	np := ExportJobNetworkPolicy("demo", "tenant-demo", Config{InfraNamespace: "gentian-infra-dev"})

	sel := np.Spec.PodSelector.MatchLabels
	if sel[meta.ComponentLabel] != "tenant-export" {
		t.Fatalf("pod selector = %v; must select only export pods", sel)
	}
	var namespaces []string
	internet := false
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector != nil {
				namespaces = append(namespaces, to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
			}
			if to.IPBlock != nil && to.IPBlock.CIDR == "0.0.0.0/0" {
				internet = true
			}
		}
	}
	// Interim: the encrypt step installs age at runtime and a tenant-namespace
	// restore fetches the MinIO client; a dedicated backup image retires this.
	if !internet {
		t.Error("missing internet egress for runtime tool installs")
	}
	want := map[string]bool{"gentian-infra-dev": false, meta.KernelNamespace: false}
	for _, ns := range namespaces {
		if _, ok := want[ns]; ok {
			want[ns] = true
		} else {
			t.Errorf("unexpected egress namespace %q", ns)
		}
	}
	for ns, seen := range want {
		if !seen {
			t.Errorf("missing egress to %q", ns)
		}
	}
}
