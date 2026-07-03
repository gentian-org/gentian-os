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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
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
