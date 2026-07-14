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
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
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
	"github.com/gentian-org/gentian-os/internal/catalogue"
)

const (
	conditionAppsReady = "AppsReady"
)

// appClaimGVK is the GVK for namespace-scoped App claims reconciled by Crossplane.
var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// ensureAppDeployment seeds OpenBao app secrets and watches Crossplane-owned App
// claims for readiness. Claim creation is owned by tenant-default Composition.
func (r *TenantReconciler) ensureAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if err := r.cleanupOrphanedAppWorkload(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned app workload: %w", err)
	}

	if len(tenant.Spec.Apps) == 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "NoAppsConfigured", "No applications are configured for this tenant")
		return ctrl.Result{}, nil
	}

	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	allReady := true

	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile, ok := appProfileFromIndex(profileIndex, profileName)
		if !ok {
			r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "ProfileNotFound",
				fmt.Sprintf("AppProfile %q not found", profileName))
			return ctrl.Result{}, nil
		}

		// ApiProfiles have no App claim to seed or await; they are always ready.
		if gentianov1alpha1.ProfileIsAPI(profile) {
			continue
		}

		if err := r.seedAppSecrets(ctx, tenant, profileName, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("seed app-secrets for %s: %w", profileName, err)
		}

		if err := r.injectLLMCredentials(ctx, tenant, profileName); err != nil {
			return ctrl.Result{}, fmt.Errorf("inject llm credentials for %s: %w", profileName, err)
		}

		ready, err := r.waitForAppClaimReady(ctx, tenant, profileName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("wait for App claim %s: %w", profileName, err)
		}
		if !ready {
			allReady = false
		}
	}

	if !allReady {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "Provisioning", "Waiting for App claims to become Ready")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "Provisioned", "All App claims are Ready")
	return ctrl.Result{}, nil
}

// seedAppSecrets writes each AppProfile.spec.appSecrets entry into OpenBao at
// …/internal/{name} with key "value". No-op when Seeder is nil or the profile
// declares no app-secrets. Repeated calls are idempotent.
func (r *TenantReconciler) seedAppSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.AppProfile) error {
	if r.Seeder == nil || len(profile.Spec.AppSecrets) == 0 {
		return nil
	}
	for _, s := range profile.Spec.AppSecrets {
		if s.Name == "" {
			continue
		}
		if _, err := r.Seeder.SeedAppSecret(ctx, tenant.Name, appName, s.Name); err != nil {
			return err
		}
	}
	for _, sidecar := range profile.Spec.Sidecars {
		scAppName := gentianov1alpha1.SidecarAppName(appName, sidecar.Name)
		for _, s := range sidecar.AppSecrets {
			if s.Name == "" {
				continue
			}
			if _, err := r.Seeder.SeedAppSecret(ctx, tenant.Name, scAppName, s.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitForAppClaimReady returns true when the Crossplane-managed App claim exists
// and its Ready condition is True.
func (r *TenantReconciler) waitForAppClaimReady(ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName string) (bool, error) {
	nsName := tenantNamespaceName(tenant)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	err := r.Get(ctx, types.NamespacedName{Name: profileName, Namespace: nsName}, obj)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return appClaimIsReady(obj), nil
}

// injectLLMCredentials creates a secret inside the tenant namespace containing the OpenAI API
// endpoints and virtual key configured by the platform.
func (r *TenantReconciler) injectLLMCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) error {
	if os.Getenv("LLM_SUPPORT") != "true" {
		return nil
	}

	nsName := tenantNamespaceName(tenant)
	secretName := fmt.Sprintf("llm-credentials-%s", appName)

	stringData := map[string]string{
		"OPENAI_API_BASE":     "http://litellm-proxy.platform-kernel.svc.cluster.local:4000/v1",
		"OPENAI_API_BASE_URL": "http://litellm-proxy.platform-kernel.svc.cluster.local:4000/v1",
		"OPENAI_API_KEY":      fmt.Sprintf("sk-gentian-%s-%s", tenant.Name, appName),
	}
	if appName == "open-webui" {
		h := sha256.New()
		h.Write([]byte(fmt.Sprintf("%s-%s-secret-salt-value", tenant.Name, appName)))
		stringData["WEBUI_SECRET_KEY"] = base64.URLEncoding.EncodeToString(h.Sum(nil))
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: stringData,
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.StringData = desired.StringData
	return r.Patch(ctx, existing, patch)
}

// deleteAppDeployment is a no-op under C1: App claims are owned by the XTenant
// Composition and deleted via deleteXTenant cascade.
func (r *TenantReconciler) deleteAppDeployment(_ context.Context, _ *gentianov1alpha1.Tenant) error {
	return nil
}

// cleanupOrphanedAppWorkload removes tenant-namespace Jobs and orphan Job pods for
// apps no longer listed in tenant.Spec.Apps. Crossplane deletes App claims on
// uninstall, but composition Jobs (e.g. catalogue-app-oidc-seed) can leave pods
// running when the owning Job disappears first.
func (r *TenantReconciler) cleanupOrphanedAppWorkload(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	desired := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return err
		}
		desired[profileName] = struct{}{}
	}

	nsName := tenantNamespaceName(tenant)
	prop := metav1.DeletePropagationBackground

	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name},
	); err != nil {
		return fmt.Errorf("list app Jobs in %s: %w", nsName, err)
	}
	for i := range jobList.Items {
		job := &jobList.Items[i]
		appName := job.Labels[appLabel]
		if appName == "" {
			continue
		}
		if _, wanted := desired[appName]; wanted {
			continue
		}
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete orphaned app Job %s: %w", job.Name, err)
		}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(nsName)); err != nil {
		return fmt.Errorf("list pods in %s: %w", nsName, err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if appName := pod.Labels[appLabel]; appName != "" {
			if _, wanted := desired[appName]; !wanted {
				if err := r.Delete(ctx, pod, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("delete pod for removed app %s: %w", pod.Name, err)
				}
				continue
			}
		}
		jobName := orphanJobNameForPod(pod)
		if jobName == "" {
			continue
		}
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: nsName}, job)
		if !errors.IsNotFound(err) {
			continue
		}
		if err := r.Delete(ctx, pod, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete orphaned Job pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

func orphanJobNameForPod(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return ref.Name
		}
	}
	if name := pod.Labels["batch.kubernetes.io/job-name"]; name != "" {
		return name
	}
	return pod.Labels["job-name"]
}

// appClaimIsReady returns true when the App claim's Ready condition is True,
// indicating Crossplane has fully reconciled the ExternalSecret and Release.
func appClaimIsReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == "True" {
			return true
		}
	}
	return false
}
