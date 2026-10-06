/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package provisioner

import (
	"context"
	"fmt"
	"strings"

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
func MatchMariaDBProfile(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile.Services() != nil &&
		profile.Services().Database != nil &&
		profile.Services().Database.Engine == gentianov1alpha1.DatabaseEngineMariaDB
}

// MatchS3Profile reports whether an AppProfile requires S3 storage provisioning.
func MatchS3Profile(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile.Services() != nil &&
		profile.Services().Storage != nil &&
		profile.Services().Storage.S3 != nil
}

// MatchRedisProfile reports whether an AppProfile requires Redis cache provisioning.
func MatchRedisProfile(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile.Services() != nil &&
		profile.Services().Cache != nil &&
		profile.Services().Cache.Engine == gentianov1alpha1.CacheEngineRedis
}

// MatchMemcachedProfile reports whether an AppProfile requires Memcached cache provisioning.
func MatchMemcachedProfile(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile.Services() != nil &&
		profile.Services().Cache != nil &&
		profile.Services().Cache.Engine == gentianov1alpha1.CacheEngineMemcached
}

// NewKernelProvisioningJob builds a standard kernel-namespace provisioning Job.
func NewKernelProvisioningJob(
	name, kernelNamespace, tenantLabelKey, managedByLabelKey, managedByValue, appLabelKey, tenantName, appName string,
	container corev1.Container,
) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
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
			ActiveDeadlineSeconds:   &deadline,
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
	var failures []string
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
		if jobFailed(existing) {
			// A cleanup Job that failed did not clean up. It used to count
			// as "not complete yet", so a deletion waited on it for ever
			// and said nothing, or went on once its TTL had removed it. It
			// is reported, and removed so the next pass runs it again: the
			// deletion resumes where it stopped and does not move past a
			// store it could not destroy.
			failures = append(failures, fmt.Sprintf("%s (%s)", name, jobFailureReason(existing)))
			prop := metav1.DeletePropagationBackground
			if err := c.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("remove the failed delete Job %s: %w", name, err)
			}
			continue
		}
		if !jobComplete(existing) {
			pending = true
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%w in %s: %s; each is run again", ErrDeleteJobFailed, kernelNamespace, strings.Join(failures, ", "))
	}
	if pending {
		return ErrDeleteJobPending
	}
	return nil
}

// ErrDeleteJobFailed is a cleanup Job that ended without doing its work.
var ErrDeleteJobFailed = fmt.Errorf("cleanup job failed")

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailureReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return strings.TrimSpace(c.Reason + " " + c.Message)
		}
	}
	return ""
}
