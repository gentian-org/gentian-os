/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const tenantProvisioningObjectsDataKey = "objects.json"

// waitForTenantNamespaceJob waits for a Batch Job in the tenant namespace.
// Used when an app Composition owns db-init / s3-init Jobs (C3.1).
func (r *TenantReconciler) waitForTenantNamespaceJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, jobName string) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: nsName}, job)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

func appUsesCrossplaneDBInit(profile *gentianov1alpha1.AppProfile) bool {
	if profile == nil || profile.Spec.CompositionRef == "" {
		return false
	}
	kr := profile.Spec.KernelRequirements
	if kr == nil || kr.Database == nil {
		return false
	}
	if kr.Database.Engine != gentianov1alpha1.DatabaseEnginePostgreSQL {
		return false
	}
	return kr.Database.DatabasePerTenant
}

func appUsesCrossplaneS3Init(profile *gentianov1alpha1.AppProfile) bool {
	if profile == nil || profile.Spec.CompositionRef == "" {
		return false
	}
	kr := profile.Spec.KernelRequirements
	if kr == nil || kr.Storage == nil || kr.Storage.S3 == nil {
		return false
	}
	return kr.Storage.S3.BucketPerTenant
}

func appCompositionInitJobName(appName, suffix string) string {
	return appName + "-" + suffix
}

func serializeProvisioningObjects(objects []client.Object) (string, error) {
	rawObjects := make([]json.RawMessage, 0, len(objects))
	for _, obj := range objects {
		uMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return "", err
		}
		if _, ok := uMap["apiVersion"]; !ok {
			gvk := obj.GetObjectKind().GroupVersionKind()
			if gvk.Empty() {
				return "", fmt.Errorf("object %T has no GroupVersionKind", obj)
			}
			uMap["apiVersion"] = gvk.GroupVersion().String()
			uMap["kind"] = gvk.Kind
		}
		raw, err := json.Marshal(uMap)
		if err != nil {
			return "", err
		}
		rawObjects = append(rawObjects, raw)
	}
	payload, err := json.Marshal(rawObjects)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
