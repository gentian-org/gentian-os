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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// Where the operator puts what a tenant needs from the platform.
//
// Each is a function of the layout, never a name. A provisioning Job runs
// beside the Secret it mounts and the service it provisions, so the namespace
// follows the function: Keycloak Jobs in the authentication namespace, role
// Jobs beside the postgres they talk to, bucket Jobs beside MinIO. The
// previous single "kernel namespace" was the v4 layout's one namespace for
// all of these, and addressing it by name is what kept every tenant out of a
// cluster built in any other shape.
//
// The system functions are the ones namespace-cleanup.md names; a cluster
// that has not composed one of them yet has no tenant using it either.
var (
	provisioningNamespace = layout.Namespace(layout.Provisioning)
	postgresNamespace     = layout.System("postgresql")
	mariadbNamespace      = layout.System("mariadb")
	s3Namespace           = layout.System("s3")
	cacheNamespace        = layout.System("cache")
	mailNamespace         = layout.System("mail")
	mailDMZNamespace      = layout.System("mail-dmz")
)

// tenantPlatformNamespaces are every platform namespace a tenant's
// provisioning leaves objects in: what a sweep on tenant deletion has to
// visit, and where a provisioning Job is looked for by name.
func tenantPlatformNamespaces() []string {
	return dedupe(
		identityNamespace,
		provisioningNamespace,
		postgresNamespace,
		mariadbNamespace,
		s3Namespace,
		cacheNamespace,
		mailNamespace,
		mailDMZNamespace,
	)
}

func isTenantPlatformNamespace(ns string) bool {
	for _, candidate := range tenantPlatformNamespaces() {
		if candidate == ns {
			return true
		}
	}
	return false
}

// getProvisioningJob finds a tenant's provisioning Job by name in whichever
// platform namespace its function put it. Names carry the function
// (keycloak-realm-, postgres-role-, s3-bucket-), so one name is at most one
// Job across them; the first namespace that has it answers.
func (r *TenantReconciler) getProvisioningJob(ctx context.Context, name string) (*batchv1.Job, error) {
	for _, ns := range tenantPlatformNamespaces() {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, job)
		if err == nil {
			return job, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, apierrors.NewNotFound(batchv1.Resource("jobs"), name)
}

// listTenantPlatformJobs lists Jobs matching opts across every platform
// namespace tenant provisioning writes to.
func (r *TenantReconciler) listTenantPlatformJobs(ctx context.Context, opts ...client.ListOption) (*batchv1.JobList, error) {
	out := &batchv1.JobList{}
	for _, ns := range tenantPlatformNamespaces() {
		page := &batchv1.JobList{}
		if err := r.List(ctx, page, append([]client.ListOption{client.InNamespace(ns)}, opts...)...); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, page.Items...)
	}
	return out, nil
}

// listTenantPlatformSecrets is listTenantPlatformJobs for Secrets.
func (r *TenantReconciler) listTenantPlatformSecrets(ctx context.Context, opts ...client.ListOption) (*corev1.SecretList, error) {
	out := &corev1.SecretList{}
	for _, ns := range tenantPlatformNamespaces() {
		page := &corev1.SecretList{}
		if err := r.List(ctx, page, append([]client.ListOption{client.InNamespace(ns)}, opts...)...); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, page.Items...)
	}
	return out, nil
}
