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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	provisioningRequeueMin = 2 * time.Second
	provisioningRequeueMax = 30 * time.Second
)

func provisioningRequeueFromJobAge(job *batchv1.Job) time.Duration {
	if job == nil || job.CreationTimestamp.IsZero() {
		return provisioningRequeueMin
	}
	delay := provisioningRequeueMin
	age := time.Since(job.CreationTimestamp.Time)
	for delay < provisioningRequeueMax && age >= delay*2 {
		delay *= 2
	}
	if delay > provisioningRequeueMax {
		return provisioningRequeueMax
	}
	return delay
}

func (r *TenantReconciler) provisioningRequeueDelay(ctx context.Context, tenantName string, jobNames ...string) time.Duration {
	delay := provisioningRequeueMin
	for _, jobName := range jobNames {
		if jobName == "" {
			continue
		}
		job, err := r.getProvisioningJob(ctx, jobName)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return delay
		}
		if jobIsComplete(job) {
			continue
		}
		if d := provisioningRequeueFromJobAge(job); d > delay {
			delay = d
		}
	}
	return delay
}

func (r *TenantReconciler) requeueForPendingJob(ctx context.Context, tenantName string, jobNames ...string) ctrl.Result {
	return ctrl.Result{RequeueAfter: r.provisioningRequeueDelay(ctx, tenantName, jobNames...)}
}

func (r *TenantReconciler) requeueForPendingApps(
	ctx context.Context,
	tenantName string,
	apps []string,
	jobName func(tenantName, appName string) string,
) ctrl.Result {
	jobNames := make([]string, 0, len(apps))
	for _, appName := range apps {
		jobNames = append(jobNames, jobName(tenantName, appName))
	}
	return r.requeueForPendingJob(ctx, tenantName, jobNames...)
}
