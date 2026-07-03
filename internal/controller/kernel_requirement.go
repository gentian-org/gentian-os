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
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// AppCollectionMode selects whether kernel app collectors read the live tenant
// spec only or also union apps inferred from historical provision Jobs.
type AppCollectionMode string

const (
	CollectForProvision AppCollectionMode = "provision"
	CollectForDelete    AppCollectionMode = "delete"
)

func (r *TenantReconciler) collectKernelApps(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	mode AppCollectionMode,
	match func(*gentianov1alpha1.AppProfile) bool,
	setupJobPrefix func(tenantName string) string,
) ([]string, error) {
	var apps []string
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if match(profile) {
			apps = appendUniqueStrings(apps, app.Profile)
		}
	}
	if mode == CollectForDelete && setupJobPrefix != nil {
		prefix := setupJobPrefix(tenant.Name)
		if prefix != "" {
			fromJobs, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, prefix)
			if err != nil {
				return nil, err
			}
			apps = appendUniqueStrings(apps, fromJobs...)
		}
	}
	return apps, nil
}

func matchMariaDBProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Database != nil &&
		profile.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEngineMariaDB
}

func matchS3Profile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Storage != nil &&
		profile.Spec.KernelRequirements.Storage.S3 != nil
}

func matchRedisProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Cache != nil &&
		profile.Spec.KernelRequirements.Cache.Engine == gentianov1alpha1.CacheEngineRedis
}

func matchMemcachedProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Cache != nil &&
		profile.Spec.KernelRequirements.Cache.Engine == gentianov1alpha1.CacheEngineMemcached
}

// jobWaitRequirement drives reconcileJobWaitRequirement for MariaDB/S3/Redis paths.
type jobWaitRequirement struct {
	conditionType string
	requeueAfter  time.Duration
	emptyReason   string
	readyReason   string
}

func (r *TenantReconciler) reconcileJobWaitRequirement(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	req jobWaitRequirement,
	collect func(context.Context, *gentianov1alpha1.Tenant, AppCollectionMode) ([]string, error),
	waitJob func(context.Context, *gentianov1alpha1.Tenant, string) (bool, error),
) (ctrl.Result, error) {
	apps, err := collect(ctx, tenant, CollectForProvision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(apps) == 0 {
		r.setCondition(tenant, req.conditionType, metav1.ConditionTrue, req.emptyReason, "No apps require provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true
	for _, appName := range apps {
		done, err := waitJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			allDone = false
		}
	}
	if !allDone {
		r.setCondition(tenant, req.conditionType, metav1.ConditionFalse, "Provisioning", "Waiting for provisioning Jobs")
		return ctrl.Result{RequeueAfter: req.requeueAfter}, nil
	}
	r.setCondition(tenant, req.conditionType, metav1.ConditionTrue, req.readyReason, "All provisioning Jobs complete")
	return ctrl.Result{}, nil
}

func newKernelProvisioningJob(name string, tenant *gentianov1alpha1.Tenant, appName string, container corev1.Container) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	labels := map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}
	if appName != "" {
		labels[appLabel] = appName
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kernelNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

func (r *TenantReconciler) ensureDeleteJobs(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	apps []string,
	jobName func(tenantName, appName string) string,
	makeJob func(*gentianov1alpha1.Tenant, string) *batchv1.Job,
) error {
	pending := false
	for _, appName := range apps {
		name := jobName(tenant.Name, appName)
		existing := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing); err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("check delete Job %s: %w", name, err)
			}
			if err := r.Create(ctx, makeJob(tenant, appName)); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create delete Job %s: %w", name, err)
			}
			pending = true
			continue
		}
		if !jobIsComplete(existing) {
			pending = true
		}
	}
	if pending {
		return errDeleteJobPending
	}
	return nil
}
