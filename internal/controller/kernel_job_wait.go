/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the permissions and limitations under the License.
*/

package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kernelJobState describes a Batch Job in platform-kernel for C2 convergence.
type kernelJobState int

const (
	kernelJobPending kernelJobState = iota
	kernelJobComplete
	kernelJobFailed
)

// waitForKernelJob returns the completion state of a Job in platform-kernel.
// Used while migrating provisioning Jobs from the operator to Crossplane Compositions.
func (r *TenantReconciler) waitForKernelJob(ctx context.Context, jobName string) (kernelJobState, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return kernelJobPending, nil
	}
	if err != nil {
		return kernelJobPending, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return kernelJobFailed, nil
	}
	if jobIsComplete(job) {
		return kernelJobComplete, nil
	}
	return kernelJobPending, nil
}
