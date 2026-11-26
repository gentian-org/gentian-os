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
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A tenant that is asleep (spec.hibernation).
//
// Everything a tenant runs is a pod in one of its three namespaces: its own,
// its DMZ and the one for runs. What it holds -- databases, buckets, cache
// users, the realm, mailboxes -- is in services the cluster shares, and its
// volumes are claims no pod has to mount for them to stay. So a tenant
// sleeps when those namespaces hold no pod, and nothing else of it is
// touched: waking it is its pods starting.
//
// Three things make that so, and the first is the one that holds:
//
//   - A ResourceQuota of no pods in each namespace. The API server then
//     refuses every pod, whoever asks for it: a Helm hook, a CronJob, a run,
//     a controller that puts replicas back. It is a quota of its own beside
//     the tenant's, so the ceiling the tenant's plan sets, and everything
//     that reads it, is as it was.
//   - Every Deployment and StatefulSet scaled to zero, and every CronJob
//     suspended, with what they were kept on them (as an export keeps a
//     replica count, and for the same reason: the workload is what has to
//     be put back). Without it they would ask for pods and be refused for
//     as long as the tenant sleeps.
//   - The pods that are left deleted: those of a Job, or of nothing.
//
// The tenant's status says it is asleep before any of that, and the
// component reconciler reads it from there: it answers at the edge for the
// tenant's apps (restore_hold.go) and rolls nothing out.
//
// A tenant is not put to sleep while one of its exports or restores runs:
// both scale apps down and up again, and one that ended after the tenant
// went to sleep would put a count back that sleep had already taken.

const (
	// hibernationQuotaName is the quota of no pods in each namespace of a
	// tenant that is asleep.
	hibernationQuotaName = "hibernation"
	// hibernationReplicasAnnotation keeps, on a workload scaled to zero for
	// sleep, the replicas it had.
	hibernationReplicasAnnotation = "gentianos.io/pre-hibernation-replicas"
	// hibernationSuspendAnnotation marks a CronJob suspended for sleep, as
	// against one that was suspended before.
	hibernationSuspendAnnotation = "gentianos.io/suspended-for-hibernation"

	conditionHibernated = "Hibernated"
	// reasonHibernated is why a component of a sleeping tenant is not Ready.
	reasonHibernated = "Hibernated"

	// hibernationRecheck is how often a sleeping tenant is looked at again.
	// The quota is what keeps it asleep; this puts back to zero what
	// something scaled up in the meantime, so that it stops asking.
	hibernationRecheck = 10 * time.Minute
	// hibernationBusyRecheck is how often a tenant that is to sleep, and has
	// an export or a restore running, is looked at again.
	hibernationBusyRecheck = 30 * time.Second
)

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;update;patch

// hibernationStep is what the tenant reconciler does next after looking at
// the tenant's sleep.
type hibernationStep struct {
	// asleep ends the reconcile: nothing else is reconciled for a tenant
	// that sleeps.
	asleep bool
	// within asks for another reconcile no later than this; zero for none.
	within time.Duration
}

// sooner is a result that comes back no later than within.
func sooner(result ctrl.Result, within time.Duration) ctrl.Result {
	if within > 0 && (result.RequeueAfter == 0 || result.RequeueAfter > within) {
		result.RequeueAfter = within
	}
	return result
}

// sleeps reports whether the operator has put the tenant to sleep and has not
// woken it yet. This, and not the spec, is what every other reconciler reads:
// it changes when the tenant reconciler acts.
func sleeps(tenant *gentianov1alpha1.Tenant) bool {
	return tenant != nil && tenant.Status.Hibernation != nil && tenant.Status.Hibernation.Asleep
}

// tenantNamespaces are the namespaces a tenant's pods run in.
func tenantNamespaces(ctx context.Context, c client.Reader, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	own, err := tenantNamespaceByName(ctx, c, tenant.Name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range []string{own, layout.TenantDMZ(tenant.Name), layout.TenantRuns(tenant.Name)} {
		ns := &corev1.Namespace{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		// Only a namespace that says it is this tenant's: the pods in it are
		// about to be deleted.
		if ns.Labels[layout.LabelTenant] != tenant.Name || ns.DeletionTimestamp != nil {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// reconcileHibernation brings a tenant to what its spec says about sleep.
func (r *TenantReconciler) reconcileHibernation(ctx context.Context, tenant *gentianov1alpha1.Tenant) (hibernationStep, error) {
	now := time.Now()
	want, was := tenant.Asleep(now), sleeps(tenant)

	switch {
	case !want && !was:
		if at, kept := tenant.FallsAsleepAt(now); kept {
			return hibernationStep{within: at.Sub(now) + time.Second}, nil
		}
		return hibernationStep{}, nil

	case !want && was:
		if err := r.wake(ctx, tenant); err != nil {
			return hibernationStep{}, fmt.Errorf("wake tenant %s: %w", tenant.Name, err)
		}
		tenant.Status.Hibernation = &gentianov1alpha1.TenantHibernationStatus{Since: &metav1.Time{Time: now}}
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
		setStatusCondition(&tenant.Status.Conditions, tenant.Generation, conditionHibernated,
			metav1.ConditionFalse, "Awake", "the tenant has been woken")
		if err := r.Status().Update(ctx, tenant); err != nil {
			return hibernationStep{}, err
		}
		log.FromContext(ctx).Info("tenant woken", "tenant", tenant.Name)
		step := hibernationStep{}
		if at, kept := tenant.FallsAsleepAt(now); kept {
			step.within = at.Sub(now) + time.Second
		}
		return step, nil
	}

	if !was {
		busy, err := r.backupRunning(ctx, tenant)
		if err != nil {
			return hibernationStep{}, err
		}
		if busy != "" {
			before := tenant.Status.DeepCopy()
			setStatusCondition(&tenant.Status.Conditions, tenant.Generation, conditionHibernated,
				metav1.ConditionFalse, "WaitingForBackup", "the tenant sleeps when "+busy+" has ended")
			if !equality.Semantic.DeepEqual(before, &tenant.Status) {
				if err := r.Status().Update(ctx, tenant); err != nil {
					return hibernationStep{}, err
				}
			}
			return hibernationStep{within: hibernationBusyRecheck}, nil
		}
	}

	// Said first: the edge answers for the tenant's apps from this, and no
	// component rolls anything out into namespaces that are being emptied.
	before := tenant.Status.DeepCopy()
	if !was {
		tenant.Status.Hibernation = &gentianov1alpha1.TenantHibernationStatus{Asleep: true, Since: &metav1.Time{Time: now}}
	}
	tenant.Status.Phase = gentianov1alpha1.TenantPhaseHibernated
	tenant.Status.ObservedGeneration = tenant.Generation
	setStatusCondition(&tenant.Status.Conditions, tenant.Generation, conditionHibernated,
		metav1.ConditionTrue, "Asleep", "the tenant is asleep: none of its pods run, and everything it holds is kept")
	if !equality.Semantic.DeepEqual(before, &tenant.Status) {
		if err := r.Status().Update(ctx, tenant); err != nil {
			return hibernationStep{}, err
		}
	}
	if !was {
		log.FromContext(ctx).Info("tenant put to sleep", "tenant", tenant.Name)
	}

	namespaces, err := tenantNamespaces(ctx, r.Client, tenant)
	if err != nil {
		return hibernationStep{}, err
	}
	for _, ns := range namespaces {
		if err := r.sleepNamespace(ctx, tenant, ns); err != nil {
			return hibernationStep{}, fmt.Errorf("put %s to sleep: %w", ns, err)
		}
	}
	return hibernationStep{asleep: true, within: hibernationRecheck}, nil
}

// backupRunning names an export or a restore of the tenant that has not
// ended, "" when there is none.
func (r *TenantReconciler) backupRunning(ctx context.Context, tenant *gentianov1alpha1.Tenant) (string, error) {
	ns, err := tenantNamespaceByName(ctx, r.Client, tenant.Name)
	if err != nil {
		return "", err
	}
	exports := &gentianov1alpha1.TenantExportList{}
	if err := r.List(ctx, exports, client.InNamespace(ns)); err != nil {
		return "", err
	}
	for i := range exports.Items {
		if e := &exports.Items[i]; e.DeletionTimestamp == nil && !e.IsTerminal() {
			return "the export " + e.Name, nil
		}
	}
	restores := &gentianov1alpha1.TenantRestoreList{}
	if err := r.List(ctx, restores, client.InNamespace(ns)); err != nil {
		return "", err
	}
	for i := range restores.Items {
		if rs := &restores.Items[i]; rs.DeletionTimestamp == nil && !rs.IsTerminal() {
			return "the restore " + rs.Name, nil
		}
	}
	return "", nil
}

// uncached reads CronJobs and pods from the API server: they are read for a
// tenant that goes to sleep or wakes, which is no reason to keep every one of
// the cluster's in memory.
func (r *TenantReconciler) uncached() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// hibernationQuota is the quota of no pods.
func hibernationQuota(tenant, namespace string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: hibernationQuotaName, Namespace: namespace,
			Labels: map[string]string{layout.LabelTenant: tenant, managedByLabel: managedByValue},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("0")},
		},
	}
}

// sleepNamespace closes a namespace to pods and takes away those it has.
func (r *TenantReconciler) sleepNamespace(ctx context.Context, tenant *gentianov1alpha1.Tenant, ns string) error {
	quota := hibernationQuota(tenant.Name, ns)
	existing := &corev1.ResourceQuota{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(quota), existing); {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, quota); err != nil {
			return err
		}
	case err != nil:
		return err
	case !equality.Semantic.DeepEqual(existing.Spec.Hard, quota.Spec.Hard):
		existing.Spec.Hard = quota.Spec.Hard
		if err := r.Update(ctx, existing); err != nil {
			return err
		}
	}

	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if err := r.sleepWorkload(ctx, d, d.Spec.Replicas, func(v *int32) { d.Spec.Replicas = v }); err != nil {
			return fmt.Errorf("deployment %s: %w", d.Name, err)
		}
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		if err := r.sleepWorkload(ctx, s, s.Spec.Replicas, func(v *int32) { s.Spec.Replicas = v }); err != nil {
			return fmt.Errorf("statefulset %s: %w", s.Name, err)
		}
	}

	cronJobs := &batchv1.CronJobList{}
	if err := r.uncached().List(ctx, cronJobs, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range cronJobs.Items {
		cj := &cronJobs.Items[i]
		if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
			continue
		}
		patch := client.MergeFrom(cj.DeepCopy())
		setObjectAnnotation(cj, hibernationSuspendAnnotation, "true")
		suspend := true
		cj.Spec.Suspend = &suspend
		if err := r.Patch(ctx, cj, patch); err != nil {
			return fmt.Errorf("cronjob %s: %w", cj.Name, err)
		}
	}

	// What is left belongs to a Job, a DaemonSet or nothing; whatever would
	// make it again is refused by the quota.
	pods := &corev1.PodList{}
	if err := r.uncached().List(ctx, pods, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// sleepWorkload scales a workload to zero and keeps the replicas it had. One
// that is at zero is left as it is: asleep already, or never running.
func (r *TenantReconciler) sleepWorkload(ctx context.Context, obj client.Object, current *int32, set func(*int32)) error {
	had := replicasOf(current)
	if had == 0 {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	// The count it has now, also when one is kept already: whoever scaled it
	// up while the tenant slept said what it is to have.
	setObjectAnnotation(obj, hibernationReplicasAnnotation, strconv.Itoa(int(had)))
	zero := int32(0)
	set(&zero)
	return r.Patch(ctx, obj, patch)
}

// wakeWorkload gives a workload the replicas sleep took from it. One sleep
// did not scale is left as it is.
func (r *TenantReconciler) wakeWorkload(ctx context.Context, obj client.Object, set func(*int32)) error {
	kept, ok := obj.GetAnnotations()[hibernationReplicasAnnotation]
	if !ok {
		return nil
	}
	// Read as the 32 bits a replica count has: an annotation is whatever
	// somebody wrote there.
	replicas, err := strconv.ParseInt(kept, 10, 32)
	if err != nil || replicas < 0 {
		replicas = 1
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	annotations := obj.GetAnnotations()
	delete(annotations, hibernationReplicasAnnotation)
	obj.SetAnnotations(annotations)
	back := int32(replicas)
	set(&back)
	return r.Patch(ctx, obj, patch)
}

func setObjectAnnotation(obj client.Object, key, value string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	obj.SetAnnotations(annotations)
}

// admitPods takes the quota of no pods out of the tenant's namespaces.
func (r *TenantReconciler) admitPods(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	namespaces, err := tenantNamespaces(ctx, r.Client, tenant)
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: hibernationQuotaName, Namespace: ns}}
		if err := r.Delete(ctx, quota); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// wake opens the tenant's namespaces to pods again and puts back what sleep
// took: the quota first, so that the replicas put back are admitted.
func (r *TenantReconciler) wake(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	namespaces, err := tenantNamespaces(ctx, r.Client, tenant)
	if err != nil {
		return err
	}
	if err := r.admitPods(ctx, tenant); err != nil {
		return err
	}
	for _, ns := range namespaces {
		deployments := &appsv1.DeploymentList{}
		if err := r.List(ctx, deployments, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range deployments.Items {
			d := &deployments.Items[i]
			if err := r.wakeWorkload(ctx, d, func(v *int32) { d.Spec.Replicas = v }); err != nil {
				return fmt.Errorf("deployment %s/%s: %w", ns, d.Name, err)
			}
		}
		statefulSets := &appsv1.StatefulSetList{}
		if err := r.List(ctx, statefulSets, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range statefulSets.Items {
			s := &statefulSets.Items[i]
			if err := r.wakeWorkload(ctx, s, func(v *int32) { s.Spec.Replicas = v }); err != nil {
				return fmt.Errorf("statefulset %s/%s: %w", ns, s.Name, err)
			}
		}

		cronJobs := &batchv1.CronJobList{}
		if err := r.List(ctx, cronJobs, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range cronJobs.Items {
			cj := &cronJobs.Items[i]
			if _, ours := cj.Annotations[hibernationSuspendAnnotation]; !ours {
				continue
			}
			patch := client.MergeFrom(cj.DeepCopy())
			delete(cj.Annotations, hibernationSuspendAnnotation)
			suspend := false
			cj.Spec.Suspend = &suspend
			if err := r.Patch(ctx, cj, patch); err != nil {
				return fmt.Errorf("cronjob %s/%s: %w", ns, cj.Name, err)
			}
		}
	}
	return nil
}
