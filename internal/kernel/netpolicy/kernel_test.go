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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func TestKernelAccessNetworkPolicy_ProfileKernelEgressNamespaces(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: "gentian-system",
			},
		},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{},
			}},
		},
	}
	np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes", profile, netpolicy.DefaultConfig())
	if np == nil {
		t.Fatal("expected network policy")
	}
	if len(np.Spec.Egress) < 2 {
		t.Fatalf("expected infra + gentian-system egress, got %d rules", len(np.Spec.Egress))
	}
}
