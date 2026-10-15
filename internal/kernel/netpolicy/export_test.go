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

	networkingv1 "k8s.io/api/networking/v1"

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
	// No way out of the cluster: a run whose bundle is at an external
	// destination is given its own (ExportDestinationNetworkPolicy).
	if internet {
		t.Error("export pods are given internet egress by the tenant's policy")
	}
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock != nil {
				t.Errorf("export pods are given egress to %s", to.IPBlock.CIDR)
			}
		}
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

// A run's way out is to its destination's port, and to its address when the
// endpoint names one; the platform's own storage is given none.
func TestExportDestinationNetworkPolicyIsTheDestinationAlone(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		cidr     string
		port     int
	}{
		{"https://sos-ch-gva-2.exo.io", "0.0.0.0/0", 443},
		{"https://s3.example.org:9000", "0.0.0.0/0", 9000},
		{"http://s3.example.org", "0.0.0.0/0", 80},
		{"https://203.0.113.7", "203.0.113.7/32", 443},
		{"http://203.0.113.7:8333/bucket", "203.0.113.7/32", 8333},
		{"https://[2001:db8::7]:9000", "2001:db8::7/128", 9000},
	} {
		np, err := ExportDestinationNetworkPolicy("demo", "tenant-demo", "p", "run", "nightly", c.endpoint)
		if err != nil || np == nil {
			t.Fatalf("%s: %v", c.endpoint, err)
		}
		if sel := np.Spec.PodSelector.MatchLabels; sel["run"] != "nightly" || sel[meta.ComponentLabel] != "tenant-export" {
			t.Errorf("%s: selects %v, not the run's export pods", c.endpoint, sel)
		}
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("%s: policy types %v", c.endpoint, np.Spec.PolicyTypes)
		}
		if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 || len(np.Spec.Egress[0].Ports) != 1 {
			t.Fatalf("%s: egress %+v, want one peer on one port", c.endpoint, np.Spec.Egress)
		}
		if got := np.Spec.Egress[0].To[0].IPBlock; got == nil || got.CIDR != c.cidr || len(got.Except) != 0 {
			t.Errorf("%s: to %+v, want %s", c.endpoint, got, c.cidr)
		}
		if got := np.Spec.Egress[0].Ports[0].Port.IntValue(); got != c.port {
			t.Errorf("%s: port %d, want %d", c.endpoint, got, c.port)
		}
		if np.Labels[meta.NetPolicyTypeLabel] != meta.NetPolicyExportDestination {
			t.Errorf("%s: labelled %v", c.endpoint, np.Labels)
		}
	}
	if np, err := ExportDestinationNetworkPolicy("demo", "tenant-demo", "p", "run", "nightly", ""); np != nil || err != nil {
		t.Errorf("the platform's own storage is given a way out: %v, %v", np, err)
	}
	for _, bad := range []string{"s3.example.org", "ftp://s3.example.org", "https://s3.example.org:0", "https://s3.example.org:99999"} {
		if _, err := ExportDestinationNetworkPolicy("demo", "tenant-demo", "p", "run", "nightly", bad); err == nil {
			t.Errorf("%q is taken for a destination", bad)
		}
	}
}
