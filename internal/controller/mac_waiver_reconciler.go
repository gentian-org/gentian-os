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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/security"
)

const (
	approvedMacWaiversAnnotation = "gentianos.io/approved-mac-waivers"
	conditionMacWaiversReady     = "MacWaiversReady"
)

// macWaiverLabelPrefix is the namespace/pod label namespace for waiver grants.
const macWaiverLabelPrefix = "mac-waiver.gentianos.io/"

// ensureMacWaiverNamespaceLabels grants approved waivers on the tenant namespace and
// reports which of them nothing has claimed yet.
//
// Kyverno needs both halves — the Pod label and this namespace label — before it
// excludes a Pod. This writes the half no chart can forge; the Pod half is the
// workload declaring which of its containers needs the exemption.
//
// Stale keys are removed, not just added: a waiver withdrawn from the
// PlatformSecurityPolicy allowlist has to stop being granted, otherwise revoking an
// approval would leave the namespace permanently exempt.
//
// The returned slice is the labels that are approved and granted here but that no
// running Pod carries, so the exemption is not yet doing anything.
func (r *TenantReconciler) ensureMacWaiverNamespaceLabels(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	approvedByProfile map[string][]gentianov1alpha1.MacWaiverRequest,
) ([]string, error) {
	wanted := map[string]struct{}{}
	for _, reqs := range approvedByProfile {
		for _, req := range reqs {
			wanted[gentianov1alpha1.MacWaiverLabelKey(req.Policy)] = struct{}{}
		}
	}

	nsName := tenantNamespaceName(tenant)
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: nsName}, ns); err != nil {
		if errors.IsNotFound(err) {
			// The namespace stage has not run yet; a later reconcile grants these.
			return nil, nil
		}
		return nil, fmt.Errorf("get namespace %s for mac waiver grants: %w", nsName, err)
	}

	desired := map[string]string{}
	for k, v := range ns.Labels {
		if strings.HasPrefix(k, macWaiverLabelPrefix) {
			continue // rebuilt below, so a revoked waiver drops out
		}
		desired[k] = v
	}
	for key := range wanted {
		desired[key] = gentianov1alpha1.MacWaiverApprovedValue
	}

	if !maps.Equal(ns.Labels, desired) {
		patch := client.MergeFrom(ns.DeepCopy())
		ns.Labels = desired
		if err := r.Patch(ctx, ns, patch); err != nil {
			return nil, fmt.Errorf("patch namespace %s mac waiver labels: %w", nsName, err)
		}
	}

	if len(wanted) == 0 {
		return nil, nil
	}

	// Visibility only. Pods are consulted to answer "is this exemption doing
	// anything", never to decide whether to grant it.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(nsName)); err != nil {
		return nil, fmt.Errorf("list pods in %s for mac waiver status: %w", nsName, err)
	}
	if len(pods.Items) == 0 {
		// No workload yet, so "not in effect" would be noise rather than a finding.
		return nil, nil
	}

	var notInEffect []string
	for key := range wanted {
		claimed := false
		for i := range pods.Items {
			if pods.Items[i].Labels[key] == gentianov1alpha1.MacWaiverApprovedValue {
				claimed = true
				break
			}
		}
		if !claimed {
			notInEffect = append(notInEffect, key)
		}
	}
	sort.Strings(notInEffect)
	return notInEffect, nil
}

// ensureMacWaivers intersects AppProfile requests with PlatformSecurityPolicy allowlist
// and records approved waivers on the Tenant for compositions to consume.
func (r *TenantReconciler) ensureMacWaivers(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	allowed, err := security.LoadAllowedMacWaivers(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	approvedByProfile := map[string][]gentianov1alpha1.MacWaiverRequest{}
	pendingDenials := []string{}

	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: profileName}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s for mac waivers: %w", profileName, err)
		}
		if profile.Spec.Security == nil || len(profile.Spec.Security.MacWaivers) == 0 {
			continue
		}
		approved := security.ApprovedMacWaivers(profileName, profile.Spec.Security.MacWaivers, allowed)
		if len(approved) > 0 {
			approvedByProfile[profileName] = approved
		}
		for _, req := range profile.Spec.Security.MacWaivers {
			if !security.IsWaiverApproved(approved, req.Policy, req.Scope) {
				pendingDenials = append(pendingDenials,
					fmt.Sprintf("%s/%s/%s", profileName, req.Policy, req.Scope))
			}
		}
	}

	payload, err := json.Marshal(approvedByProfile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal approved mac waivers: %w", err)
	}
	encoded := string(payload)
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	if tenant.Annotations[approvedMacWaiversAnnotation] != encoded {
		patch := client.MergeFrom(tenant.DeepCopy())
		tenant.Annotations[approvedMacWaiversAnnotation] = encoded
		if err := r.Patch(ctx, tenant, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch tenant mac waiver annotations: %w", err)
		}
	}

	// The half that actually grants the exemption.
	//
	// Recording approvals on the Tenant is not enforcement: nothing reads that
	// annotation. Kyverno excludes a Pod only when the Pod carries
	// mac-waiver.gentianos.io/<policy>=approved AND its namespace carries the same
	// key — and the namespace half is written here, from the intersection above.
	//
	// Splitting it that way is the point. The pod label alone used to be the whole
	// check, so any chart could exempt itself and the PlatformSecurityPolicy
	// allowlist was never consulted. Namespace labels are writable only by this
	// operator, so a forged pod label now achieves nothing on its own.
	notInEffect, err := r.ensureMacWaiverNamespaceLabels(ctx, tenant, approvedByProfile)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case len(pendingDenials) > 0:
		r.setCondition(tenant, conditionMacWaiversReady, metav1.ConditionFalse, "WaiverNotApproved",
			fmt.Sprintf("MAC waivers pending cluster approval: %v", pendingDenials))
	case len(notInEffect) > 0:
		// Approved and granted at the namespace, but no running Pod claims it, so
		// nothing is actually exempt yet. Reported rather than left silent: the
		// alternative is a Tenant that reads Approved while its workload is still
		// being denied, which is the state that made this whole mechanism look
		// like it worked.
		r.setCondition(tenant, conditionMacWaiversReady, metav1.ConditionFalse, "AwaitingWorkloadOptIn",
			fmt.Sprintf("approved, but no Pod carries the waiver label yet — add %v to the workload pod template",
				notInEffect))
	default:
		r.setCondition(tenant, conditionMacWaiversReady, metav1.ConditionTrue, "Approved",
			"All requested MAC waivers are approved or none requested")
	}
	return ctrl.Result{}, nil
}
