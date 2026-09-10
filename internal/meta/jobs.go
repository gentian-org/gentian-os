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

package meta

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ProvisioningJobTTLSeconds is how long finished Batch Jobs remain before the
// TTL controller deletes them. Keep short so tenant CPU quotas free quickly.
const ProvisioningJobTTLSeconds int32 = 600

// ProvisioningJobBackoffLimit allows dependent identity Jobs to survive transient
// failures while keycloak-realm-* is still running (Jobs are applied in parallel).
const ProvisioningJobBackoffLimit int32 = 12

// ProvisioningJobActiveDeadlineSeconds is the wall-clock ceiling on a
// configuration Job, retries included.
//
// Without one a Job runs until something outside Kubernetes stops it. Every
// script here talks to Keycloak, Postgres, Redis or S3 over the network, and a
// peer that accepts the connection and then goes quiet gives a command that
// never returns, a pod that never exits, and a reconcile that waits on that Job
// with nothing to time it out. The Job reports neither success nor failure, so
// the tenant simply stops progressing.
//
// An hour is chosen against the retry budget, not the work: these Jobs finish in
// seconds, but ActiveDeadlineSeconds covers all attempts, and Kubernetes backs
// off 10s, 20s, 40s, 80s, 160s, then 360s per attempt. At the default six
// retries that is about eleven minutes; at ProvisioningJobBackoffLimit's twelve
// it is roughly forty-six. An hour clears both, so this only ever fires on a Job
// that is genuinely stuck rather than one still retrying.
//
// Deliberately not applied to backup, restore or purge Jobs. Those move data and
// can legitimately run for hours, so they need a bound derived from the volume
// they are moving, not this one.
const ProvisioningJobActiveDeadlineSeconds int64 = 3600

// InitJobResources caps CPU/memory for operator-owned provisioning Jobs in
// platform-kernel.
//
// The values are fixed here. This used to say tenant-scoped init Jobs read
// tenant.initJob.* from gentian-cluster-config via the app-default composition,
// which was not true of any composition: app-default reads one key from that
// ConfigMap and it is not this one. The claim fields those keys came from have
// been removed, so the only sizing is the one below.
func InitJobResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// SecretEnv builds an EnvVar sourced from one key of one Secret.
//
// It lived twice, byte-identical, in internal/backup and internal/applifecycle —
// the two packages that build Jobs needing a credential. Both already import
// this package for InitJobResources, so it costs nothing to share.
func SecretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
	}
}
