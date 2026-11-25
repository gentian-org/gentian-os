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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/kernelnet"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The way out of the cluster an export or a restore is given: to the
// external destination its bundle is at, from its own pods, while it runs.
// In the tenant's namespace, where every pod is kept in by default. And in a
// kernel namespace a unit runs in that keeps its pods inside the cluster
// (kernel-egress): the desktop's database is captured in kernel-data, and
// its upload went nowhere. A namespace that restricts no egress is given
// nothing: a policy there would restrict the run's pods to the destination
// alone. A run whose bundle stays in the platform's storage is given none;
// the tenant's export policy opens no way out at all.

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
// the run, and the same in the kernel namespaces its units run in that keep
// their pods inside the cluster; and takes it away from a run that needs none.
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
		if err := discardRunDestinationEgress(ctx, c, run.GetNamespace(), kind, run.GetName()); err != nil {
			return err
		}
	}
	if err := ensureRunDestinationEgressBeside(ctx, c, run, kind, bundle, units); err != nil {
		return err
	}
	if desired == nil {
		return nil
	}
	if err := controllerutil.SetControllerReference(run, desired, scheme); err != nil {
		return fmt.Errorf("own the destination's NetworkPolicy %s: %w", name, err)
	}
	err = ensureObject(ctx, c, desired, func(existing *networkingv1.NetworkPolicy) bool {
		existing.Labels = desired.Labels
		existing.OwnerReferences = desired.OwnerReferences
		existing.Spec = desired.Spec
		return true
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure NetworkPolicy %s: %w", name, err)
	}
	return nil
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
	return discardRunDestinationEgressBeside(ctx, c, tenantNameFromNamespace(namespace), kind, run, nil)
}

// sharedRunEgressPolicyName is the policy one run is given in a namespace
// every tenant's runs share: by the tenant too, as a run's name is unique
// in its tenant only.
func sharedRunEgressPolicyName(tenant, kind, run string) string {
	return runEgressPolicyName(kind, tenant+"-"+run)
}

// runEgressLabels mark a run's policy in a shared namespace, which the run
// cannot own across namespaces: they are how it is found and removed.
func runEgressLabels(tenant, kind, run string) map[string]string {
	return map[string]string{
		meta.NetPolicyTypeLabel: meta.NetPolicyExportDestination,
		meta.TenantLabel:        tenant,
		backup.ExportLabel:      run,
		backup.RunKindLabel:     kind,
	}
}

// ensureRunDestinationEgressBeside gives the run its way out in every kernel
// namespace one of its units runs in whose pods are kept inside the cluster,
// and takes it away from the namespaces it no longer needs one in.
func ensureRunDestinationEgressBeside(
	ctx context.Context,
	c client.Client,
	run client.Object,
	kind string,
	bundle *gentianov1alpha1.BundleRef,
	units []captureUnit,
) error {
	tenant := tenantNameFromNamespace(run.GetNamespace())
	name := sharedRunEgressPolicyName(tenant, kind, run.GetName())
	var wanted []string
	if bundle != nil && bundle.Endpoint != "" {
		for _, ns := range unitNamespaces(units) {
			if ns == run.GetNamespace() {
				continue
			}
			kept, err := keepsPodsInside(ctx, c, ns)
			if err != nil {
				return err
			}
			if !kept {
				continue
			}
			desired, err := netpolicy.ExportDestinationNetworkPolicy(tenant, ns, name, backup.ExportLabel, run.GetName(), bundle.Endpoint)
			if err != nil {
				return err
			}
			// Another tenant's run of the same name runs here too: only
			// this tenant's pods.
			desired.Spec.PodSelector.MatchLabels[meta.TenantLabel] = tenant
			for k, v := range runEgressLabels(tenant, kind, run.GetName()) {
				desired.Labels[k] = v
			}
			err = ensureObject(ctx, c, desired, func(existing *networkingv1.NetworkPolicy) bool {
				existing.Labels = desired.Labels
				existing.Spec = desired.Spec
				return true
			})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("ensure NetworkPolicy %s/%s: %w", ns, name, err)
			}
			wanted = append(wanted, ns)
		}
	}
	return discardRunDestinationEgressBeside(ctx, c, tenant, kind, run.GetName(), wanted)
}

// keepsPodsInside reports whether a namespace keeps its pods inside the
// cluster: it has the kernel's egress policy.
func keepsPodsInside(ctx context.Context, c client.Client, ns string) (bool, error) {
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: kernelnet.EgressPolicyName}, &networkingv1.NetworkPolicy{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// discardRunDestinationEgressBeside removes the run's policies in shared
// namespaces, except in the namespaces kept.
func discardRunDestinationEgressBeside(ctx context.Context, c client.Client, tenant, kind, run string, kept []string) error {
	list := &networkingv1.NetworkPolicyList{}
	if err := c.List(ctx, list, client.MatchingLabels(runEgressLabels(tenant, kind, run))); err != nil {
		return err
	}
	for i := range list.Items {
		np := &list.Items[i]
		if slices.Contains(kept, np.Namespace) {
			continue
		}
		if err := c.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

const (
	runKindExport  = "export"
	runKindRestore = "restore"
)
