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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// An App claim reporting Ready does not mean the app is running.
//
// The Release that installs it is created with wait: false, so provider-helm
// reports Ready as soon as `helm install` returns — deliberately, because
// waiting blocks the reconcile on the app's own start-up. function-auto-ready
// then propagates that up, and the Tenant said "All App claims are Ready"
// while the workload underneath had never admitted a single pod.
//
// That is not a hypothetical: a tenant whose quota was one CPU short of its
// installed apps ran for fifteen hours with Nextcloud entirely absent, every
// claim Ready, and the only trace a FailedCreate on a ReplicaSet nobody reads.
// The quota was then raised, and nothing happened — which is the second half
// of the problem below.

const (
	// quotaObservedAnnotation records which ResourceQuota a Deployment has
	// already been given a chance under. It lives on the POD TEMPLATE because
	// writing it is also the nudge: changing the template rolls a new
	// ReplicaSet, and a new ReplicaSet is what actually retries the create.
	quotaObservedAnnotation = "gentianos.io/quota-observed"

	tenantQuotaName = "tenant-quota"
)

// quotaFingerprint is a stable digest of a quota's hard limits.
//
// Compared against what a blocked Deployment last saw, it answers the only
// question worth acting on: has the ceiling changed since this was refused?
func quotaFingerprint(hard corev1.ResourceList) string {
	keys := make([]string, 0, len(hard))
	for k := range hard {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		q := hard[corev1.ResourceName(k)]
		// Discarded explicitly: a hash.Hash never fails a write, and errcheck
		// wants the intent stated rather than inferred.
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, q.String())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// deploymentBlocked reports whether a Deployment is failing to create pods,
// and why. ReplicaFailure is how the ReplicaSet controller records a create
// the API server refused — quota, admission webhook, or PodSecurity.
func deploymentBlocked(d *appsv1.Deployment) (bool, string) {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return true, c.Message
		}
	}
	return false, ""
}

// reconcileAppWorkloadHealth finds workloads that cannot create pods, retries
// the ones the cluster has since made room for, and returns what is still
// stuck so the caller can say so instead of reporting the tenant healthy.
//
// Why a nudge is needed at all: nothing retries a quota-refused ReplicaSet
// when the quota is later raised. The ReplicaSet keeps its ReplicaFailure, its
// events age out, and a widened ResourceQuota is not an event the ReplicaSet
// controller watches — so the fix an operator just applied appears to do
// nothing, which is how it gets mistaken for a broken app.
//
// The nudge is bounded by the fingerprint: a blocked Deployment is retried
// once per distinct quota, never in a loop. When the quota has not moved,
// nothing is written and nothing rolls, because nothing has changed that
// would make the retry succeed. The cost of that precision is that room
// freed by *uninstalling* another app does not trigger a retry on its own;
// the next quota edit, or a manual rollout restart, does.
func (r *TenantReconciler) reconcileAppWorkloadHealth(ctx context.Context, ns string) ([]string, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
	}

	// No quota means no ceiling to compare against; a workload blocked for some
	// other reason is still reported, just never nudged from here.
	fingerprint := ""
	var rq corev1.ResourceQuota
	switch err := r.Get(ctx, types.NamespacedName{Name: tenantQuotaName, Namespace: ns}, &rq); {
	case err == nil:
		fingerprint = quotaFingerprint(rq.Spec.Hard)
	case client.IgnoreNotFound(err) != nil:
		return nil, fmt.Errorf("get %s/%s: %w", ns, tenantQuotaName, err)
	}

	var stuck []string
	for i := range deployments.Items {
		d := &deployments.Items[i]
		blocked, message := deploymentBlocked(d)
		if !blocked {
			continue
		}
		stuck = append(stuck, fmt.Sprintf("%s (%s)", d.Name, firstLine(message)))

		if fingerprint == "" || d.Spec.Template.Annotations[quotaObservedAnnotation] == fingerprint {
			continue
		}

		patched := d.DeepCopy()
		if patched.Spec.Template.Annotations == nil {
			patched.Spec.Template.Annotations = map[string]string{}
		}
		patched.Spec.Template.Annotations[quotaObservedAnnotation] = fingerprint
		if err := r.Patch(ctx, patched, client.MergeFrom(d)); err != nil {
			return nil, fmt.Errorf("retry blocked deployment %s/%s: %w", ns, d.Name, err)
		}
		log.FromContext(ctx).Info("retrying a workload that could not create pods; the quota has changed since it was refused",
			"namespace", ns, "deployment", d.Name, "refusal", firstLine(message))
	}

	sort.Strings(stuck)
	return stuck, nil
}

// firstLine keeps a condition message to its useful part — the API server's
// refusals carry the whole quota accounting, which is worth logging once but
// not worth repeating in a status message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
