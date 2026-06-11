/*
Copyright 2026 The Gentian Authors.

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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionCacheReady   = "CacheReady"
	redisProvisionerImage = "redis:7-alpine"
	redisAdminSecret      = "redis-admin"
	cacheRequeueAfter     = 2 * time.Second
	argocdGroup           = "argoproj.io"
	argocdVersion         = "v1alpha1"
	argocdApplicationKind = "Application"
	argocdNamespace       = "argocd"
)

// Memcached chart coordinates — configurable via Helm values / env vars so
// upgrades don't require an operator image rebuild.
var (
	memcachedChartRepo    = envOrDefault("MEMCACHED_CHART_REPO", "https://charts.bitnami.com/bitnami")
	memcachedChartName    = envOrDefault("MEMCACHED_CHART_NAME", "memcached")
	memcachedChartVersion = envOrDefault("MEMCACHED_CHART_VERSION", "8.6.1")
)

var argocdApplicationGVK = schema.GroupVersionKind{
	Group:   argocdGroup,
	Version: argocdVersion,
	Kind:    argocdApplicationKind,
}

// ensureCache provisions per-app Redis ACL users (via redis-cli Job) and per-tenant
// Memcached instances (via ArgoCD Application CR). CacheReady is set to True once all
// Jobs complete and all Application CRs report Healthy.
func (r *TenantReconciler) ensureCache(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	redisApps, memcachedApps, err := r.collectCacheApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(redisApps) == 0 && len(memcachedApps) == 0 {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionTrue,
			"NoCacheRequired", "No apps require cache provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true

	// One Redis ACL Job per app.
	for _, appName := range redisApps {
		done, err := r.ensureRedisACLJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Redis ACL Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
		}
	}

	// One Memcached ArgoCD Application CR per tenant (covers all apps needing Memcached).
	if len(memcachedApps) > 0 {
		done, err := r.ensureMemcachedApplication(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Memcached Application CR: %w", err)
		}
		if !done {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for cache resources to be ready")
		return ctrl.Result{RequeueAfter: cacheRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionCacheReady, metav1.ConditionTrue,
		"Provisioned", "All cache resources are ready")
	return ctrl.Result{}, nil
}

// collectCacheApps inspects AppProfiles and partitions apps by cache engine.
func (r *TenantReconciler) collectCacheApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) (redisApps, memcachedApps []string, err error) {
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Cache == nil {
			continue
		}
		switch profile.Spec.KernelRequirements.Cache.Engine {
		case gentianov1alpha1.CacheEngineRedis:
			redisApps = append(redisApps, app.Profile)
		case gentianov1alpha1.CacheEngineMemcached:
			memcachedApps = append(memcachedApps, app.Profile)
		}
	}
	return redisApps, memcachedApps, nil
}

// ensureRedisACLJob creates (or checks completion of) the Redis ACL SETUSER Job for
// a single app. Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureRedisACLJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	jobName := redisACLJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		// Inc 21a: derive the per-app Redis ACL password and persist it under
		// the canonical OpenBao path before creating the redis-cli Job. The
		// Job receives the same value via REDIS_USER_PASSWORD so the live ACL
		// and OpenBao stay in lockstep. When Seeder is nil the Job falls back
		// to using the admin password (legacy behaviour).
		userPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedCache(ctx, tenant.Name, appName, secrets.CacheCreds{
				Host: fmt.Sprintf("%s.%s.svc.cluster.local", "redis-master", kernelNamespace),
				Port: "6379",
			})
			if seedErr != nil {
				return false, fmt.Errorf("seed cache: %w", seedErr)
			}
			userPassword = creds.Password
		}
		return false, r.Create(ctx, makeRedisACLJob(tenant, appName, userPassword))
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

// ensureMemcachedApplication creates (or checks health of) the ArgoCD Application CR
// that deploys a per-tenant Memcached instance. Returns true when health=Healthy.
func (r *TenantReconciler) ensureMemcachedApplication(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	appName := memcachedApplicationName(tenant.Name)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	err := r.Get(ctx, types.NamespacedName{Name: appName, Namespace: argocdNamespace}, obj)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, buildMemcachedApplication(tenant))
	}
	if err != nil {
		return false, err
	}
	currentRev, _, _ := unstructured.NestedString(obj.Object, "spec", "source", "targetRevision")
	if currentRev != memcachedChartVersion {
		_ = unstructured.SetNestedField(obj.Object, memcachedChartVersion, "spec", "source", "targetRevision")
		if err := r.Update(ctx, obj); err != nil {
			return false, fmt.Errorf("update memcached chart version: %w", err)
		}
		return false, nil
	}
	return argocdApplicationIsHealthy(obj), nil
}

// deleteCache handles cache cleanup on tenant deletion.
// DeletionPolicy=Delete:
//   - Creates ACL DELUSER Jobs for per-app Redis users.
//   - Deletes the ArgoCD Application CR so ArgoCD removes the Memcached deployment.
//
// DeletionPolicy=Retain:
//   - No-op — Redis keys and Memcached data are preserved for recovery.
func (r *TenantReconciler) deleteCache(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	redisApps, memcachedApps, err := r.collectCacheAppsForDelete(ctx, tenant)
	if err != nil {
		return err
	}

	pending := false
	for _, appName := range redisApps {
		jobName := redisACLDeleteJobName(tenant.Name, appName)
		existing := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing); err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("check Redis ACL delete Job %s: %w", jobName, err)
			}
			if err := r.Create(ctx, makeRedisACLDeleteJob(tenant, appName)); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create Redis ACL delete Job %s: %w", jobName, err)
			}
			pending = true
		} else if !jobIsComplete(existing) {
			pending = true
		}
	}

	if len(memcachedApps) > 0 {
		appCR := &unstructured.Unstructured{}
		appCR.SetGroupVersionKind(argocdApplicationGVK)
		appCR.SetName(memcachedApplicationName(tenant.Name))
		appCR.SetNamespace(argocdNamespace)
		if err := r.Delete(ctx, appCR); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Memcached Application CR: %w", err)
		}
	} else {
		appCR := &unstructured.Unstructured{}
		appCR.SetGroupVersionKind(argocdApplicationGVK)
		appCR.SetName(memcachedApplicationName(tenant.Name))
		appCR.SetNamespace(argocdNamespace)
		if err := r.Get(ctx, types.NamespacedName{Name: appCR.GetName(), Namespace: appCR.GetNamespace()}, appCR); err == nil {
			if err := r.Delete(ctx, appCR); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete Memcached Application CR: %w", err)
			}
		} else if !errors.IsNotFound(err) {
			return fmt.Errorf("get Memcached Application CR: %w", err)
		}
	}
	if pending {
		return errDeleteJobPending
	}
	return nil
}

// --- Job constructors --------------------------------------------------------

// makeRedisACLJob creates a redis-cli Job that provisions a per-app Redis ACL user.
// The script is idempotent: ACL SETUSER creates or overwrites the user entry.
func makeRedisACLJob(tenant *gentianov1alpha1.Tenant, appName, userPassword string) *batchv1.Job {
	ttl := int32(3600)
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
	ttl := int32(3600)
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

// --- ArgoCD Application CR constructor --------------------------------------

// buildMemcachedApplication returns an ArgoCD Application CR that deploys a
// Bitnami Memcached chart into the tenant's namespace. ArgoCD handles deployment
// and lifecycle; the orchestrator only creates/deletes the Application CR.
func buildMemcachedApplication(tenant *gentianov1alpha1.Tenant) *unstructured.Unstructured {
	nsName := tenantNamespaceName(tenant)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	obj.SetName(memcachedApplicationName(tenant.Name))
	obj.SetNamespace(argocdNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(obj.Object, memcachedChartRepo, "spec", "source", "repoURL")
	_ = unstructured.SetNestedField(obj.Object, memcachedChartName, "spec", "source", "chart")
	_ = unstructured.SetNestedField(obj.Object, memcachedChartVersion, "spec", "source", "targetRevision")
	_ = unstructured.SetNestedField(obj.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "destination", "namespace")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "prune")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "selfHeal")
	return obj
}

// --- Container constructor ---------------------------------------------------

// redisContainer returns a Container running redis:7-alpine with admin credentials
// injected from redis-admin Secret and user-specific values as literal env vars.
func redisContainer(name, username, keyPrefix, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   redisProvisionerImage,
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
func redisSetUserScript(username, keyPrefix string) string {
	return fmt.Sprintf(
		`set -euo pipefail
USER_PW="${REDIS_USER_PASSWORD:-$REDIS_PASSWORD}"
redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning \
  ACL SETUSER %s on ">$USER_PW" "~%s" "+@read" "+@write" "+@connection"
echo "ACL user %s provisioned"`,
		username, keyPrefix, username,
	)
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

// --- Status helper -----------------------------------------------------------

// argocdApplicationIsHealthy returns true when the ArgoCD Application CR reports
// status.health.status == "Healthy". Used to gate CacheReady=True for Memcached tenants.
func argocdApplicationIsHealthy(obj *unstructured.Unstructured) bool {
	status, found, err := unstructured.NestedString(obj.Object, "status", "health", "status")
	if err != nil || !found {
		return false
	}
	return status == "Healthy"
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

// memcachedApplicationName returns the ArgoCD Application CR name for a tenant's Memcached instance.
func memcachedApplicationName(tenantName string) string {
	return fmt.Sprintf("memcached-%s", tenantName)
}

func redisACLJobName(tenantName, appName string) string {
	return fmt.Sprintf("redis-acl-%s-%s", tenantName, appName)
}

func redisACLDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("redis-acl-delete-%s-%s", tenantName, appName)
}
