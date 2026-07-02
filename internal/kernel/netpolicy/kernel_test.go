// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package netpolicy_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func TestKernelAccessNetworkPolicy_ProfileKernelEgressNamespaces(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: "gentian-system",
			},
		},
		Spec: gentianov1alpha1.AppProfileSpec{
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{},
			},
		},
	}
	np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "app-store", profile, netpolicy.DefaultConfig())
	if np == nil {
		t.Fatal("expected network policy")
	}
	if len(np.Spec.Egress) < 2 {
		t.Fatalf("expected infra + gentian-system egress, got %d rules", len(np.Spec.Egress))
	}
}
