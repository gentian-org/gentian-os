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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

	litellmMasterKeySecret = "llm-sensitive-values"
	litellmMasterKeyNS     = "platform-kernel"
	litellmMasterKeySecKey = "litellm_master_key" //nolint:gosec // Secret key name, not a credential.
)

// litellmProxyBaseURL is a var rather than a const only so tests can point it at
// an httptest server. Nothing at run time reassigns it.
var litellmProxyBaseURL = "http://litellm-proxy.platform-kernel.svc.cluster.local:4000"

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

		if err := r.injectLLMCredentials(ctx, tenant, profileName, profile); err != nil {
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

	// Every claim is Ready, which is a statement about Helm, not about the
	// app. Ask the workloads before repeating it — see app_workload_health.go
	// for what a Ready claim is worth on its own.
	stuck, err := r.reconcileAppWorkloadHealth(ctx, tenantNamespaceName(tenant))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check app workload health: %w", err)
	}
	if len(stuck) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "WorkloadCannotStart",
			fmt.Sprintf("Installed, but cannot create pods: %s", strings.Join(stuck, "; ")))
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
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

// derivedSecretValue computes a stable per-tenant, per-app value.
//
// The formula is frozen — see DerivedSecretKey. It predates the declaration and
// is reproduced exactly, so making the key declarative does not rotate it and
// does not invalidate the sessions of any app already using one.
func derivedSecretValue(tenantName, appName string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s-%s-secret-salt-value", tenantName, appName)
	return base64.URLEncoding.EncodeToString(h.Sum(nil))
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
// endpoints and virtual key configured by the platform, and registers that same virtual key
// with LiteLLM itself (idempotent — see ensureLiteLLMVirtualKey) so it is actually usable
// rather than a string the proxy has never heard of.
func (r *TenantReconciler) injectLLMCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.AppProfile) error {
	if !clusterLLMEnabled(ctx, r.Client) {
		return nil
	}

	nsName := tenantNamespaceName(tenant)
	secretName := fmt.Sprintf("llm-credentials-%s", appName)
	virtualKey := fmt.Sprintf("sk-gentian-%s-%s", tenant.Name, appName)

	stringData := map[string]string{
		"OPENAI_API_BASE":     litellmProxyBaseURL + "/v1",
		"OPENAI_API_BASE_URL": litellmProxyBaseURL + "/v1",
		"OPENAI_API_KEY":      virtualKey,
	}
	// Extra deterministic keys the profile asked for. The platform does not know
	// which apps need one — spec.derivedSecretKeys says so.
	if profile != nil {
		for _, dsk := range profile.Spec.DerivedSecretKeys {
			stringData[dsk.Key] = derivedSecretValue(tenant.Name, appName)
		}
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
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Labels = desired.Labels
		existing.StringData = desired.StringData
		if err := r.Patch(ctx, existing, patch); err != nil {
			return err
		}
	}

	masterKey, err := r.getLiteLLMMasterKey(ctx)
	if err != nil {
		return fmt.Errorf("read LiteLLM master key: %w", err)
	}
	keyAlias := fmt.Sprintf("%s-%s", tenant.Name, appName)
	return ensureLiteLLMVirtualKey(ctx, masterKey, virtualKey, keyAlias)
}

// getLiteLLMMasterKey reads the admin key LiteLLM itself was seeded with
// (scripts/seed-openbao.sh → ExternalSecret llm-sensitive-values in
// platform-kernel), needed to call its key-management API as an admin.
func (r *TenantReconciler) getLiteLLMMasterKey(ctx context.Context) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: litellmMasterKeySecret, Namespace: litellmMasterKeyNS}, secret); err != nil {
		return "", err
	}
	key := string(secret.Data[litellmMasterKeySecKey])
	if key == "" {
		return "", fmt.Errorf("secret %s/%s has no %q key", litellmMasterKeyNS, litellmMasterKeySecret, litellmMasterKeySecKey)
	}
	return key, nil
}

// ensureLiteLLMVirtualKey registers virtualKey with LiteLLM's own key database if it isn't
// already known, via LiteLLM's admin key-management API (GET /key/info to check, POST
// /key/generate to create — LiteLLM supports a caller-supplied `key` value, it does not
// only generate random ones). Without this, a virtual key computed by injectLLMCredentials
// authenticates against nothing: LiteLLM returns 401 token_not_found_in_db for any request
// using it, however correct the Secret otherwise looks — confirmed live against this
// cluster's open-webui deployment before this function existed.
func ensureLiteLLMVirtualKey(ctx context.Context, masterKey, virtualKey, keyAlias string) error {
	exists, err := litellmKeyExists(ctx, masterKey, virtualKey)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"key":       virtualKey,
		"key_alias": keyAlias,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, litellmProxyBaseURL+"/key/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+masterKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LiteLLM /key/generate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("LiteLLM /key/generate status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// litellmKeyExists checks LiteLLM's key database via GET /key/info?key=... — 200 means the
// key is already registered (nothing to do), 404 means it genuinely is not (create it), any
// other status is a real error worth surfacing (bad master key, proxy unreachable, etc.).
func litellmKeyExists(ctx context.Context, masterKey, virtualKey string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, litellmProxyBaseURL+"/key/info?key="+virtualKey, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("LiteLLM /key/info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("LiteLLM /key/info status %d: %s", resp.StatusCode, string(respBody))
	}
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
