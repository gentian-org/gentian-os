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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/kernelnet"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// An export or a restore whose bundle is at an external destination is
// given a way to it from its own pods in the tenant's namespace -- that
// destination's port, owned by the run -- and loses it when it ends. One
// whose bundle stays in the platform's storage is given none.
func TestARunIsGivenAWayOutOnlyToItsExternalDestination(t *testing.T) {
	ctx := context.Background()
	for _, external := range []bool{false, true} {
		w := newPlacementWorld(t, external)
		w.exportUnits(t, gentianov1alpha1.ExportEncryptionRecipient)
		w.restoreUnits(t, gentianov1alpha1.ExportEncryptionPassphrase)
		for _, run := range []struct {
			kind  string
			owner client.Object
		}{{runKindExport, w.export}, {runKindRestore, w.restore}} {
			np := &networkingv1.NetworkPolicy{}
			err := w.c.Get(ctx, types.NamespacedName{Name: runEgressPolicyName(run.kind, run.owner.GetName()), Namespace: "tenant-acme"}, np)
			if !external {
				if !apierrors.IsNotFound(err) {
					t.Errorf("%s to the platform's storage: given a way out (%v)", run.kind, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s to an external destination: no way out: %v", run.kind, err)
			}
			if sel := np.Spec.PodSelector.MatchLabels; sel[backup.ExportLabel] != run.owner.GetName() || sel[meta.ComponentLabel] != "tenant-export" {
				t.Errorf("%s: the policy selects %v, not the run's own pods", run.kind, sel)
			}
			if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 1 || np.Spec.Egress[0].Ports[0].Port.IntValue() != 443 {
				t.Errorf("%s: the way out is not the destination's port 443: %+v", run.kind, np.Spec.Egress)
			}
			if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].Name != run.owner.GetName() {
				t.Errorf("%s: the policy is not owned by its run: %+v", run.kind, np.OwnerReferences)
			}
			if np.Labels[meta.NetPolicyTypeLabel] != meta.NetPolicyExportDestination {
				t.Errorf("%s: the policy is not marked as a run's: %v", run.kind, np.Labels)
			}
		}
		if !external {
			continue
		}
		// Ended, the run is given no way out any longer.
		if err := w.er.discardStagedSecrets(ctx, w.export); err != nil {
			t.Fatal(err)
		}
		if err := w.rr.discardStagedRestoreSecrets(ctx, w.restore); err != nil {
			t.Fatal(err)
		}
		for kind, name := range map[string]string{runKindExport: w.export.Name, runKindRestore: w.restore.Name} {
			err := w.c.Get(ctx, types.NamespacedName{Name: runEgressPolicyName(kind, name), Namespace: "tenant-acme"}, &networkingv1.NetworkPolicy{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("%s ended and keeps its way out (%v)", kind, err)
			}
		}
	}
}

// A run whose units all run beside the platform's stores is given no way
// out of the tenant's namespace, whatever its destination.
func TestARunWithNoUnitInTheTenantsNamespaceIsGivenNoWayOut(t *testing.T) {
	ctx := context.Background()
	w := newPlacementWorld(t, true)
	units := []captureUnit{{JobName: "x", Job: backup.BundleDeleteJob(backup.JobParams{Name: "x", Namespace: s3Namespace, Tenant: "acme", Export: "export-1"})}}
	if err := ensureRunDestinationEgress(ctx, w.c, w.er.Scheme, w.export, runKindExport, w.export.Status.Bundle, units); err != nil {
		t.Fatal(err)
	}
	err := w.c.Get(ctx, types.NamespacedName{Name: runEgressPolicyName(runKindExport, "export-1"), Namespace: "tenant-acme"}, &networkingv1.NetworkPolicy{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("given a way out with no unit in the tenant's namespace (%v)", err)
	}
}

// A restore and a bundle's deletion address the bundle as it was written:
// the style recorded on it, not the policy's today.
func TestARestoreAddressesTheBundleAsItWasWritten(t *testing.T) {
	w := newPlacementWorld(t, true)
	w.restore.Status.Bundle.AddressingStyle = gentianov1alpha1.S3AddressingVirtualHost
	p := w.rr.jobParams(w.tenant, "wiki", w.restore)
	if p.AddressingStyle != gentianov1alpha1.S3AddressingVirtualHost {
		t.Errorf("the restore's Jobs address the bundle %q", p.AddressingStyle)
	}
	w.export.Status.Bundle.AddressingStyle = gentianov1alpha1.S3AddressingVirtualHost
	if p := w.er.jobParams(w.tenant, "wiki", w.export, backup.Encryption{}); p.AddressingStyle != gentianov1alpha1.S3AddressingVirtualHost {
		t.Errorf("the export's Jobs address the bundle %q", p.AddressingStyle)
	}
}

// A unit that runs in a kernel namespace keeping its pods inside the cluster
// -- the desktop's database in kernel-data -- is given the way out there too:
// for this tenant's pods of this run alone, removed when the run ends. A
// namespace that restricts no egress is given nothing, as a policy there
// would keep the run's pods from everything but the destination.
func TestARunIsGivenItsWayOutInAKernelNamespaceThatKeepsPodsIn(t *testing.T) {
	ctx := context.Background()
	w := newPlacementWorld(t, true)
	if err := w.c.Create(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: kernelnet.EgressPolicyName, Namespace: "kernel-data"}}); err != nil {
		t.Fatal(err)
	}
	unit := func(ns string) captureUnit {
		return captureUnit{JobName: "u-" + ns, Job: backup.BundleDeleteJob(backup.JobParams{Name: "u-" + ns, Namespace: ns, Tenant: "acme", Export: "export-1"})}
	}
	units := []captureUnit{unit("kernel-data"), unit("system-postgresql")}
	if err := ensureRunDestinationEgress(ctx, w.c, w.er.Scheme, w.export, runKindExport, w.export.Status.Bundle, units); err != nil {
		t.Fatal(err)
	}
	name := sharedRunEgressPolicyName("acme", runKindExport, "export-1")
	np := &networkingv1.NetworkPolicy{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: name, Namespace: "kernel-data"}, np); err != nil {
		t.Fatalf("no way out in kernel-data: %v", err)
	}
	sel := np.Spec.PodSelector.MatchLabels
	if sel[meta.TenantLabel] != "acme" || sel[backup.ExportLabel] != "export-1" || sel[meta.ComponentLabel] != "tenant-export" {
		t.Errorf("the way out selects %v, not this tenant's pods of this run", sel)
	}
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 1 || np.Spec.Egress[0].Ports[0].Port.IntValue() != 443 {
		t.Errorf("the way out is %+v, not the destination's port alone", np.Spec.Egress)
	}
	err := w.c.Get(ctx, types.NamespacedName{Name: name, Namespace: "system-postgresql"}, &networkingv1.NetworkPolicy{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("a namespace restricting no egress was given a policy (%v)", err)
	}

	// Ended, the run keeps no way out in the shared namespace either.
	if err := w.er.discardStagedSecrets(ctx, w.export); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Get(ctx, types.NamespacedName{Name: name, Namespace: "kernel-data"}, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Errorf("the run ended and keeps its way out of kernel-data (%v)", err)
	}
}
