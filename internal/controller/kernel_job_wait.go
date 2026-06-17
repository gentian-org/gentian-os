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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var crossplaneObjectGVK = schema.GroupVersionKind{
	Group:   "kubernetes.crossplane.io",
	Version: "v1alpha2",
	Kind:    "Object",
}

func provisioningJobObjectName(tenantName, jobName string) string {
	return fmt.Sprintf("%s-job-%s", tenantName, jobName)
}

func crossplaneObjectReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		if typ != "Ready" {
			continue
		}
		status, _ := cond["status"].(string)
		return status == string(metav1.ConditionTrue)
	}
	return false
}

// waitForProvisioningJob returns true when a Crossplane-owned provisioning Job
// in platform-kernel has completed successfully. When the Batch Job was removed
// by TTL after success, the corresponding kubernetes.crossplane.io/Object MR is
// consulted so tenants do not stay stuck in Provisioning.
func (r *TenantReconciler) waitForProvisioningJob(ctx context.Context, tenantName, jobName string) (bool, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if err == nil {
		if jobIsFailed(job) {
			prop := metav1.DeletePropagationBackground
			_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			return false, nil
		}
		return jobIsComplete(job), nil
	}
	if !errors.IsNotFound(err) {
		return false, err
	}
	if tenantName == "" {
		return false, nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crossplaneObjectGVK)
	objName := provisioningJobObjectName(tenantName, jobName)
	if err := r.Get(ctx, types.NamespacedName{Name: objName}, obj); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return crossplaneObjectReady(obj), nil
}
