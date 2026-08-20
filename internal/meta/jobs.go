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
