/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The policy must be scoped to export pods and grant exactly the two
// namespaces a capture needs. An empty pod selector here would hand every
// tenant workload egress to the infra namespace — the opposite of the MAC
// floor the baseline establishes.
func TestExportJobNetworkPolicyIsScopedToExportPods(t *testing.T) {
	np := ExportJobNetworkPolicy("demo", "tenant-demo", Config{})

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
	// A volume's capture and restore run here and reach a bundle destination
	// a backup policy names, wherever its endpoint is.
	if !internet {
		t.Error("missing internet egress to an external bundle destination")
	}
	want := map[string]bool{layout.System("postgresql"): false, layout.System("mariadb"): false, layout.System("s3"): false}
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
