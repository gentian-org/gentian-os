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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The way out of the cluster an export or a restore is given: to the
// external destination its bundle is at, from its own pods in the tenant's
// namespace, while it runs. A run whose bundle stays in the platform's
// storage, or whose pods all run beside the platform's stores, is given
// none; the tenant's export policy opens no way out at all.

// runEgressPolicyName is the policy one run is given: by the kind of run, so
// that an export and a restore of one name are two policies, and by its
// name, cut to a digest where the two would not fit a name.
func runEgressPolicyName(kind, run string) string {
	prefix := "gentian-" + kind + "-destination-"
	if len(prefix)+len(run) <= 253 {
		return prefix + run
	}
	sum := sha256.Sum256([]byte(run))
	return prefix + hex.EncodeToString(sum[:])[:32]
}

// ensureRunDestinationEgress gives a run whose units include one in the
// tenant's namespace its way to the bundle's external destination, owned by
// the run; and takes it away from a run that needs none.
func ensureRunDestinationEgress(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	run client.Object,
	kind string,
	bundle *gentianov1alpha1.BundleRef,
	units []captureUnit,
) error {
	name := runEgressPolicyName(kind, run.GetName())
	endpoint := ""
	if bundle != nil && slices.Contains(unitNamespaces(units), run.GetNamespace()) {
		endpoint = bundle.Endpoint
	}
	desired, err := netpolicy.ExportDestinationNetworkPolicy(tenantNameFromNamespace(run.GetNamespace()),
		run.GetNamespace(), name, backup.ExportLabel, run.GetName(), endpoint)
	if err != nil {
		return err
	}
	if desired == nil {
		return discardRunDestinationEgress(ctx, c, run.GetNamespace(), kind, run.GetName())
	}
	if err := controllerutil.SetControllerReference(run, desired, scheme); err != nil {
		return fmt.Errorf("own the destination's NetworkPolicy %s: %w", name, err)
	}
	existing := &networkingv1.NetworkPolicy{}
	err = c.Get(ctx, types.NamespacedName{Name: name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("open the way to the bundle's destination: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.Spec = desired.Spec
	return c.Patch(ctx, existing, patch)
}

// discardRunDestinationEgress closes a run's way out: on every path a run
// ends by, beside its staged copies. Its owner reference would remove it
// with the run; a finished run is kept, and its policy is not.
func discardRunDestinationEgress(ctx context.Context, c client.Client, namespace, kind, run string) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: runEgressPolicyName(kind, run), Namespace: namespace,
	}}
	if err := c.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

const (
	runKindExport  = "export"
	runKindRestore = "restore"
)
