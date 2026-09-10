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

// Jobs the operator no longer creates, and the sweep that removes them.
//
// A provisioning Job is retired when the last object it wrote becomes a
// Composition resource. What is left of it by then is usually nothing — an admin
// token, a lookup, and a read-back of what the Composition already made — but a
// retired Job is not harmless where it sits. Its TTL expires, an older operator
// image or a stale Argo revision recreates it, and it writes again against
// objects that now have a declared owner.
//
// So the names outlive the Jobs, purely to delete what they left behind on
// clusters that ran them.

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// tenantBrokerIdPJobName wrote the kernel IdP, the tenant realm's
// first-broker-login flow, and the two mappers that carry gentian_username
// across the realm boundary. Each moved to tenant-default in turn, until the
// script had nothing left to write.
func tenantBrokerIdPJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-idp-%s", tenantName)
}

// oidcBrowserFlowJobName bound the realm's browser flow and login theme, which
// the composed Realm declares. What remained hunted a legacy browser-kernel-idp
// flow that is no longer in any realm.
func oidcBrowserFlowJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-oidc-browser-%s", tenantName)
}

// brokerFirstLoginFlowJobName built the tenant's first-broker-login flow, which
// the Composition owns. What remained deleted every user's kernel
// federated-identity link on every run, with no test for staleness — a one-time
// migration that was never taken out.
func brokerFirstLoginFlowJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-first-login-%s", tenantName)
}

// browserSecurityJobNames are the two Jobs that wrote X-Frame-Options and the
// rest of the browser security headers. The tenant realm's are the composed
// Realm's `securityDefenses`; the kernel realm's are applied in-process, because
// no XTenant covers that realm.
func kernelBrowserSecurityJobName() string {
	return "keycloak-browser-security-kernel"
}

func tenantBrowserSecurityJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-browser-security-%s", tenantName)
}

// deleteRetiredJobs removes provisioning Jobs the operator no longer creates.
//
// NotFound is the expected outcome, not an error: on any cluster that never ran
// the retired Job, or has already been swept, there is nothing to delete.
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
