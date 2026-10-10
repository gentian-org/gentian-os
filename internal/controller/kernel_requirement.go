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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
)

// AppCollectionMode is re-exported from the shared provisioner package.
type AppCollectionMode = provisioner.AppCollectionMode

const (
	CollectForProvision = provisioner.CollectForProvision
	CollectForDelete    = provisioner.CollectForDelete
)

// collectKernelApps is the apps of a tenant that have one kind of store.
//
// For provisioning that is the apps the tenant has now whose profile
// declares the store (match). For a deletion it is every app the store was
// ever made for: those, the ones the tenant's record of what was provisioned
// names (recorded) -- which is how the stores of an app uninstalled long ago
// are found -- and, while they last, the ones a setup Job still names.
func (r *TenantReconciler) collectKernelApps(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	mode AppCollectionMode,
	match func(*gentianov1alpha1.ComponentProfile) bool,
	setupJobPrefix func(tenantName string) string,
	recorded func(backup.Provisioned) bool,
) ([]string, error) {
	profileIndex, err := loadComponentProfileIndex(ctx, r.Client)
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
	if mode == CollectForDelete && recorded != nil {
		fromRecord, err := r.recordedApps(ctx, tenant.Name, recorded)
		if err != nil {
			return nil, err
		}
		apps = appendUniqueStrings(apps, fromRecord...)
	}
	return apps, nil
}

func matchMariaDBProfile(profile *gentianov1alpha1.ComponentProfile) bool {
	return provisioner.MatchMariaDBProfile(profile)
}

func matchS3Profile(profile *gentianov1alpha1.ComponentProfile) bool {
	return provisioner.MatchS3Profile(profile)
}

func matchRedisProfile(profile *gentianov1alpha1.ComponentProfile) bool {
	return provisioner.MatchRedisProfile(profile)
}

func matchMemcachedProfile(profile *gentianov1alpha1.ComponentProfile) bool {
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

func newKernelProvisioningJob(name, namespace string, tenant *gentianov1alpha1.Tenant, appName string, container corev1.Container) *batchv1.Job {
	return provisioner.NewKernelProvisioningJob(
		name, namespace, tenantLabel, managedByLabel, managedByValue, appLabel,
		tenant.Name, appName, container,
	)
}

func (r *TenantReconciler) ensureDeleteJobs(
	ctx context.Context,
	namespace string,
	tenant *gentianov1alpha1.Tenant,
	apps []string,
	jobName func(tenantName, appName string) string,
	makeJob func(*gentianov1alpha1.Tenant, string) *batchv1.Job,
) error {
	return provisioner.EnsureDeleteJobs(ctx, r.Client, namespace, tenant, apps, jobName, makeJob, jobIsComplete)
}
