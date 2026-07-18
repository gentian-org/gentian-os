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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
)

// AppCollectionMode is re-exported from the shared provisioner package.
type AppCollectionMode = provisioner.AppCollectionMode

const (
	CollectForProvision = provisioner.CollectForProvision
	CollectForDelete    = provisioner.CollectForDelete
)

func (r *TenantReconciler) collectKernelApps(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	mode AppCollectionMode,
	match func(*gentianov1alpha1.AppProfile) bool,
	setupJobPrefix func(tenantName string) string,
) ([]string, error) {
	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, app := range tenant.Spec.Apps {
		profile, ok := appProfileFromIndex(profileIndex, app.Profile)
		if !ok {
			continue
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
	return provisioner.MatchMariaDBProfile(profile)
}

func matchS3Profile(profile *gentianov1alpha1.AppProfile) bool {
	return provisioner.MatchS3Profile(profile)
}

func matchRedisProfile(profile *gentianov1alpha1.AppProfile) bool {
	return provisioner.MatchRedisProfile(profile)
}

func matchMemcachedProfile(profile *gentianov1alpha1.AppProfile) bool {
	return provisioner.MatchMemcachedProfile(profile)
}

type jobWaitRequirement struct {
	conditionType string
	emptyReason   string
	readyReason   string
	jobNameForApp func(tenantName, appName string) string
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
		return r.requeueForPendingApps(ctx, tenant.Name, apps, req.jobNameForApp), nil
	}
	r.setCondition(tenant, req.conditionType, metav1.ConditionTrue, req.readyReason, "All provisioning Jobs complete")
	return ctrl.Result{}, nil
}

func newKernelProvisioningJob(name string, tenant *gentianov1alpha1.Tenant, appName string, container corev1.Container) *batchv1.Job {
	return provisioner.NewKernelProvisioningJob(
		name, kernelNamespace, tenantLabel, managedByLabel, managedByValue, appLabel,
		tenant.Name, appName, container,
	)
}

func (r *TenantReconciler) ensureDeleteJobs(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	apps []string,
	jobName func(tenantName, appName string) string,
	makeJob func(*gentianov1alpha1.Tenant, string) *batchv1.Job,
) error {
	return provisioner.EnsureDeleteJobs(ctx, r.Client, kernelNamespace, tenant, apps, jobName, makeJob, jobIsComplete)
}
