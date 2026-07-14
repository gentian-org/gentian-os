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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionCacheReady     = "CacheReady"
	redisAdminSecret        = "redis-admin"
	cacheRequeueAfter       = 2 * time.Second
	memcachedServiceName    = "memcached"
	memcachedDeploymentName = "memcached"
	memcachedPort           = int32(11211)
)

// ensureCache provisions per-app Redis ACL users (via redis-cli Job) and per-tenant
// Memcached instances (via Deployment + Service named "memcached"). CacheReady is set
// to True once all Jobs complete and Memcached reports ready replicas.
func (r *TenantReconciler) ensureCache(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	redisApps, memcachedApps, err := r.collectCacheApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(redisApps) == 0 && len(memcachedApps) == 0 {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionTrue,
			"NoCacheRequired", "No apps require cache provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true
	var pendingJobs []string

	// One Redis ACL Job per app.
	for _, appName := range redisApps {
		done, err := r.ensureRedisACLJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Redis ACL Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
			pendingJobs = append(pendingJobs, redisACLJobName(tenant.Name, appName))
		}
	}

	// One Memcached Deployment per tenant (covers all apps needing Memcached).
	if len(memcachedApps) > 0 {
		done, err := r.ensureMemcached(ctx, tenant, memcachedApps)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Memcached: %w", err)
		}
		if !done {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for cache resources to be ready")
		return r.requeueForPendingJob(ctx, tenant.Name, pendingJobs...), nil
	}

	r.setCondition(tenant, conditionCacheReady, metav1.ConditionTrue,
		"Provisioned", "All cache resources are ready")
	return ctrl.Result{}, nil
}

// collectCacheApps inspects AppProfiles and partitions apps by cache engine.
func (r *TenantReconciler) collectCacheApps(ctx context.Context, tenant *gentianov1alpha1.Tenant, mode AppCollectionMode) (redisApps, memcachedApps []string, err error) {
	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, nil, err
	}
	for _, app := range tenant.Spec.Apps {
		profile, ok := appProfileFromIndex(profileIndex, app.Profile)
		if !ok {
			continue
		}
		if profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Cache == nil {
			continue
		}
		switch profile.Spec.KernelRequirements.Cache.Engine {
		case gentianov1alpha1.CacheEngineRedis:
			if matchRedisProfile(profile) {
				redisApps = appendUniqueStrings(redisApps, app.Profile)
			}
		case gentianov1alpha1.CacheEngineMemcached:
			if matchMemcachedProfile(profile) {
				memcachedApps = appendUniqueStrings(memcachedApps, app.Profile)
			}
		}
	}
	if mode == CollectForDelete {
		fromJobs, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, redisACLJobName(tenant.Name, ""))
		if err != nil {
			return nil, nil, err
		}
		redisApps = appendUniqueStrings(redisApps, fromJobs...)
	}
	return redisApps, memcachedApps, nil
}

// ensureRedisACLJob waits for the Crossplane-owned Redis ACL Job.
func (r *TenantReconciler) ensureRedisACLJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, redisACLJobName(tenant.Name, appName))
}

// ensureMemcached waits for the Crossplane-provisioned Memcached Deployment.
func (r *TenantReconciler) ensureMemcached(ctx context.Context, tenant *gentianov1alpha1.Tenant, memcachedApps []string) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	dep := &appsv1.Deployment{}
	depKey := types.NamespacedName{Name: memcachedDeploymentName, Namespace: nsName}
	err := r.Get(ctx, depKey, dep)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return deploymentIsReady(dep), nil
}

// deleteCache handles cache cleanup on tenant deletion.
// DeletionPolicy=Delete:
//   - Creates ACL DELUSER Jobs for per-app Redis users.
//   - Deletes the Memcached Deployment and Service.
//
// DeletionPolicy=Retain:
//   - No-op — Redis keys and Memcached data are preserved for recovery.
func (r *TenantReconciler) deleteCache(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	redisApps, memcachedApps, err := r.collectCacheApps(ctx, tenant, CollectForDelete)
	if err != nil {
		return err
	}

	if err := r.ensureDeleteJobs(ctx, tenant, redisApps, redisACLDeleteJobName, makeRedisACLDeleteJob); err != nil {
		return err
	}

	nsName := tenantNamespaceName(tenant)
	prop := metav1.DeletePropagationBackground
	deleteOpts := &client.DeleteOptions{PropagationPolicy: &prop}

	if len(memcachedApps) > 0 {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: memcachedDeploymentName, Namespace: nsName},
		}
		if err := r.Delete(ctx, dep, deleteOpts); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Memcached Deployment: %w", err)
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: memcachedServiceName, Namespace: nsName},
		}
		if err := r.Delete(ctx, svc, deleteOpts); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Memcached Service: %w", err)
		}
	}

	return nil
}

// --- Job constructors --------------------------------------------------------

// makeRedisACLJob creates a redis-cli Job that provisions a per-app Redis ACL user.
// The script is idempotent: ACL SETUSER creates or overwrites the user entry.
func makeRedisACLJob(tenant *gentianov1alpha1.Tenant, appName, userPassword string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	username := redisACLUsername(tenant.Name, appName)
	keyPrefix := redisKeyPrefix(tenant.Name, appName)
	c := redisContainer("set-acl-user", username, keyPrefix, redisSetUserScript(username, keyPrefix))
	if userPassword != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "REDIS_USER_PASSWORD", Value: userPassword})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisACLJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

// makeRedisACLDeleteJob creates a redis-cli Job that removes the per-app Redis ACL user.
func makeRedisACLDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	username := redisACLUsername(tenant.Name, appName)
	keyPrefix := redisKeyPrefix(tenant.Name, appName)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisACLDeleteJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						redisContainer("del-acl-user", username, keyPrefix, redisDelUserScript(username)),
					},
				},
			},
		},
	}
}

// --- Memcached workload constructors -----------------------------------------

func makeMemcachedDeployment(tenant *gentianov1alpha1.Tenant) *appsv1.Deployment {
	nsName := tenantNamespaceName(tenant)
	replicas := int32(1)
	podLabels := map[string]string{
		"app.kubernetes.io/name":     "memcached",
		"app.kubernetes.io/instance": memcachedDeploymentName,
		meta.ComponentLabel:          meta.TenantCacheComponentValue,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memcachedDeploymentName,
			Namespace: nsName,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "memcached",
				"app.kubernetes.io/instance": memcachedDeploymentName,
				tenantLabel:                  tenant.Name,
				managedByLabel:               managedByValue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "memcached",
							Image: kernel.MemcachedImage(),
							Ports: []corev1.ContainerPort{
								{
									Name:          "memcached",
									ContainerPort: memcachedPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

func makeMemcachedService(tenant *gentianov1alpha1.Tenant) *corev1.Service {
	nsName := tenantNamespaceName(tenant)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memcachedServiceName,
			Namespace: nsName,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "memcached",
				"app.kubernetes.io/instance": memcachedDeploymentName,
				tenantLabel:                  tenant.Name,
				managedByLabel:               managedByValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name":     "memcached",
				"app.kubernetes.io/instance": memcachedDeploymentName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "memcached",
					Port:       memcachedPort,
					TargetPort: intstr.FromInt32(memcachedPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// --- Container constructor ---------------------------------------------------

// redisContainer returns a Container running redis:7-alpine with admin credentials
// injected from redis-admin Secret and user-specific values as literal env vars.
func redisContainer(name, username, keyPrefix, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   kernel.RedisProvisionerImage(),
		Command: []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{
			{
				Name: "REDIS_HOST",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: redisAdminSecret},
						Key:                  "host",
					},
				},
			},
			{
				Name: "REDIS_PORT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: redisAdminSecret},
						Key:                  "port",
					},
				},
			},
			{
				Name: "REDIS_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: redisAdminSecret},
						Key:                  "password",
					},
				},
			},
			// Per-user values as safe literals.
			{Name: "REDIS_USERNAME", Value: username},
			{Name: "REDIS_KEY_PREFIX", Value: keyPrefix},
		},
	}
}

// --- Shell scripts -----------------------------------------------------------

// redisSetUserScript returns an idempotent ACL SETUSER script.
// ACL SETUSER is safe to re-run — it resets the ACL entry to the given rules.
// allkeys is required when session keys are not scoped to a single key prefix (some PHP apps).
func redisSetUserScript(username, _ string) string {
	return fmt.Sprintf(
		`set -euo pipefail
USER_PW="${REDIS_USER_PASSWORD:-$REDIS_PASSWORD}"
redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning \
  ACL SETUSER %s on ">$USER_PW" allkeys "+@read" "+@write" "+@connection" "+eval" "+evalsha" "+@pubsub"
echo "ACL user %s provisioned"`,
		username, username,
	)
}

// redisCacheEndpoint returns the shared Redis host/port from the kernel redis-admin Secret.
func (r *TenantReconciler) redisCacheEndpoint(ctx context.Context) (host, port string, err error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: redisAdminSecret, Namespace: kernelNamespace}, secret); err != nil {
		return "", "", fmt.Errorf("get redis-admin secret: %w", err)
	}
	host = string(secret.Data["host"])
	port = string(secret.Data["port"])
	if port == "" {
		port = "6379"
	}
	if host == "" {
		return "", "", fmt.Errorf("redis-admin secret missing host")
	}
	return host, port, nil
}

// redisDelUserScript returns a script that removes the ACL user, ignoring absence.
func redisDelUserScript(username string) string {
	return fmt.Sprintf(
		`set -euo pipefail
redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning \
  ACL DELUSER %s 2>/dev/null || echo "user %s already absent"
echo "done"`,
		username, username,
	)
}

// --- Status helpers ----------------------------------------------------------

func deploymentIsReady(dep *appsv1.Deployment) bool {
	if dep.Spec.Replicas == nil {
		return false
	}
	desired := *dep.Spec.Replicas
	return dep.Status.ReadyReplicas >= desired &&
		dep.Status.UpdatedReplicas >= desired &&
		dep.Status.AvailableReplicas >= desired
}

// --- Name helpers ------------------------------------------------------------

// redisACLUsername returns the Redis ACL username for a tenant + app.
// Redis usernames have no strict character restrictions, but we keep them
// to [a-z0-9-] to match the convention used in other provisioning Jobs.
func redisACLUsername(tenantName, appName string) string {
	return fmt.Sprintf("%s-%s", tenantName, appName)
}

// redisKeyPrefix returns the Redis key prefix scoped to a tenant + app.
// ACL key patterns use glob syntax; the trailing * is part of the ACL rule,
// not the prefix value stored here.
func redisKeyPrefix(tenantName, appName string) string {
	return fmt.Sprintf("%s:%s:", tenantName, appName)
}

func redisACLJobName(tenantName, appName string) string {
	return fmt.Sprintf("redis-acl-%s-%s", tenantName, appName)
}

func redisACLDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("redis-acl-delete-%s-%s", tenantName, appName)
}
