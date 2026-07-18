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

package provisioner

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// ErrDeleteJobPending indicates a cleanup Job has been created but not finished.
var ErrDeleteJobPending = fmt.Errorf("cleanup job not yet complete")

// AppCollectionMode selects whether kernel app collectors read the live tenant
// spec only or also union apps inferred from historical provision Jobs.
type AppCollectionMode string

const (
	CollectForProvision AppCollectionMode = "provision"
	CollectForDelete    AppCollectionMode = "delete"
)

// JobWaitRequirement drives ReconcileJobWaitRequirement for MariaDB/S3/Redis paths.
type JobWaitRequirement struct {
	ConditionType string
	EmptyReason   string
	ReadyReason   string
	JobNameForApp func(tenantName, appName string) string
}

// MatchMariaDBProfile reports whether an AppProfile requires MariaDB provisioning.
func MatchMariaDBProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Database != nil &&
		profile.Spec.KernelRequirements.Database.Engine == gentianov1alpha1.DatabaseEngineMariaDB
}

// MatchS3Profile reports whether an AppProfile requires S3 storage provisioning.
func MatchS3Profile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Storage != nil &&
		profile.Spec.KernelRequirements.Storage.S3 != nil
}

// MatchRedisProfile reports whether an AppProfile requires Redis cache provisioning.
func MatchRedisProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Cache != nil &&
		profile.Spec.KernelRequirements.Cache.Engine == gentianov1alpha1.CacheEngineRedis
}

// MatchMemcachedProfile reports whether an AppProfile requires Memcached cache provisioning.
func MatchMemcachedProfile(profile *gentianov1alpha1.AppProfile) bool {
	return profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Cache != nil &&
		profile.Spec.KernelRequirements.Cache.Engine == gentianov1alpha1.CacheEngineMemcached
}

// NewKernelProvisioningJob builds a standard kernel-namespace provisioning Job.
func NewKernelProvisioningJob(
	name, kernelNamespace, tenantLabelKey, managedByLabelKey, managedByValue, appLabelKey, tenantName, appName string,
	container corev1.Container,
) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	labels := map[string]string{
		tenantLabelKey:    tenantName,
		managedByLabelKey: managedByValue,
	}
	if appName != "" {
		labels[appLabelKey] = appName
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

// EnsureDeleteJobs creates delete Jobs for each app when missing and returns
// ErrDeleteJobPending while any Job is incomplete.
func EnsureDeleteJobs(
	ctx context.Context,
	c client.Client,
	kernelNamespace string,
	tenant *gentianov1alpha1.Tenant,
	apps []string,
	jobName func(tenantName, appName string) string,
	makeJob func(*gentianov1alpha1.Tenant, string) *batchv1.Job,
	jobComplete func(*batchv1.Job) bool,
) error {
	pending := false
	for _, appName := range apps {
		name := jobName(tenant.Name, appName)
		existing := &batchv1.Job{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing); err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("check delete Job %s: %w", name, err)
			}
			if err := c.Create(ctx, makeJob(tenant, appName)); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create delete Job %s: %w", name, err)
			}
			pending = true
			continue
		}
		if !jobComplete(existing) {
			pending = true
		}
	}
	if pending {
		return ErrDeleteJobPending
	}
	return nil
}
