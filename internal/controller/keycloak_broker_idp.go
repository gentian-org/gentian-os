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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// firstBrokerLoginFlowAlias is a tenant-realm authentication flow that auto-links
// kernel IdP logins to pre-provisioned users by email (no confirm/re-auth).
const firstBrokerLoginFlowAlias = "first-broker-login-gentian"

// tenantBrokerIdPJobName names a Job that no longer exists.
//
// It once wrote the kernel IdP, the tenant realm's first-broker-login flow, and
// the two mappers that carry gentian_username across the realm boundary. Each of
// those moved to tenant-default in turn — the IdP, then the flow, then the
// mappers — until the script had nothing left to write and only read back what
// the Composition had already made.
//
// The name survives to delete what it left behind on clusters that ran it.
func tenantBrokerIdPJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-idp-%s", tenantName)
}

// deleteRetiredJobs removes provisioning Jobs the operator no longer creates.
//
// A retired Job is not harmless where it sits: it is a second writer of objects
// the Composition owns, and an old operator image or a Job that outlived its TTL
// will happily run it again.
func (r *TenantReconciler) deleteRetiredJobs(ctx context.Context, names ...string) {
	logger := log.FromContext(ctx)
	for _, name := range names {
		job := &batchv1.Job{}
		job.Name = name
		job.Namespace = kernelNamespace
		if err := r.Delete(ctx, job); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "delete retired provisioning job", "job", name)
		}
	}
}
