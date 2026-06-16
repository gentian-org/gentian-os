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

package controller_test

import (
	"context"
	"encoding/json"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const tenantProvisioningJobsConfigType = "tenant-provisioning-jobs"

// startTenantProvisioningJobSimulator creates Batch Jobs from the operator's
// tenant-*-provisioning-jobs ConfigMap. Envtest has no Crossplane; this mimics
// tenant-default identity Job Objects for controller integration tests.
func startTenantProvisioningJobSimulator(ctx context.Context, c client.Client) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				simulateTenantProvisioningJobsOnce(ctx, c)
			}
		}
	}()
}

func simulateTenantProvisioningJobsOnce(ctx context.Context, c client.Client) {
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace("platform-kernel"),
		client.MatchingLabels{"gentianos.io/config-type": tenantProvisioningJobsConfigType}); err != nil {
		return
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		payload := cm.Data["jobs.json"]
		if payload == "" {
			continue
		}
		var jobs []batchv1.Job
		if err := json.Unmarshal([]byte(payload), &jobs); err != nil {
			continue
		}
		for j := range jobs {
			job := jobs[j]
			if job.Namespace == "" {
				job.Namespace = "platform-kernel"
			}
			createIfMissing(ctx, c, &job)
		}
	}
}
