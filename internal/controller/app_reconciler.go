/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	"k8s.io/apimachinery/pkg/api/meta"
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
	litellmMasterKeySecKey = "litellm_master_key" //nolint:gosec // Secret key name, not a credential.
)

// Where LLM serving runs.
//
// Both of these were the literal platform-kernel, which is v4's one namespace
// for everything. LLM serving is a system-tier function of its own
// (namespace-cleanup.md §2.2), and on v5 platform-kernel does not exist -- so
// the master key was read from a namespace that is not there and the proxy was
// addressed at a name that does not resolve. Neither failure names LLM: the
// first is a Secret not found, the second a dial timeout.
//
// litellmProxyBaseURL is a var rather than a const only so tests can point it
// at an httptest server. Nothing at run time reassigns it.
var litellmMasterKeyNS = llmNamespace

var litellmProxyBaseURL = fmt.Sprintf("http://litellm-proxy.%s.svc.cluster.local:4000", llmNamespace)

// appClaimGVK is the GVK for namespace-scoped App claims reconciled by Crossplane.
var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// reconcileTenantApps reports whether a tenant's apps are up, and writes the two
// things the Composition cannot write for itself.
//
// It does NOT deploy anything, despite what it was called for a long time. App
// claims are created by the tenant-default Composition, each claim drives the
// app Composition, and that emits the ExternalSecret and the provider-helm
// Release. The operator once created a per-app Argo CD Application here; that
// path is gone, and the name outlived it.
//
// What is left splits in two, and they are kept visibly apart because only one
// of them is meant to stay:
//
//   - seedAppPrerequisites — the operator's remaining WRITES. OpenBao app
//     secrets and the LiteLLM virtual key. Both exist because a Composition
//     cannot mint a credential and store it; both belong in the Composition, or
//     behind a Managed Resource, once there is a mechanism for it. Until then
//     the claim's ExternalSecret has nothing to resolve unless this runs first.
//   - the loop below — pure status AGGREGATION. Claim readiness, then workload
//     health, then one condition on the Tenant.
//
// When the writes move, what remains is the aggregation, and this becomes a
// read-only reconciler.
func (r *TenantReconciler) reconcileTenantApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if err := r.cleanupOrphanedAppWorkload(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned app workload: %w", err)
	}

	// Every installed app is a Component, and the Component is what installs
	// it. Before the empty case below, because a tenant whose last app was
	// removed still has that app's Component to take away.
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure app components: %w", err)
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

		// An API entry runs nothing, so there is nothing to seed for it. Its
		// Component still says whether it is ready.
		if !profile.IsAPI() {
			if err := r.seedAppPrerequisites(ctx, tenant, profileName, profile); err != nil {
				return ctrl.Result{}, err
			}
		}

		ready, err := r.appComponentReady(ctx, tenant, profileName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("read the %s component: %w", profileName, err)
		}
		if !ready {
			allReady = false
		}
	}

	// Asked BEFORE allReady is acted on, not after.
	//
	// This used to sit below the `if !allReady` return, so the retry only ran
	// once every App claim was already Ready — and a claim is not Ready while
	// its Helm release is not, and a release is not ready while its Deployment
	// cannot create pods. The one thing that restarts such a Deployment was
	// therefore unreachable in exactly the state it exists to clear:
	//
	//   quota refuses the pod
	//     → Deployment exhausts progressDeadlineSeconds and stops trying
	//       → Helm release never becomes ready
	//         → App claim never becomes Ready
	//           → allReady is false, so the retry never runs
	//
	// Raising the quota then appeared to do nothing, because nothing was
	// watching. A tenant sat like that until a deploy timed out and took the
	// whole tenant down with it.
	//
	// Running it unconditionally costs one List per reconcile and is otherwise
	// inert: the nudge is still bounded by the quota fingerprint, so a workload
	// is retried once per distinct ceiling and never in a loop.
	stuck, err := r.reconcileAppWorkloadHealth(ctx, tenantNamespaceName(tenant))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check app workload health: %w", err)
	}

	if !allReady {
		// Which workloads cannot start, when any cannot. "Waiting for App
		// claims" is true and says nothing an operator can act on; a claim
		// pending because a pod was refused reads identically to one pending
		// because Helm is still installing, and they need different responses.
		msg := "Waiting for the installed apps' components to become Ready"
		if len(stuck) > 0 {
			msg = fmt.Sprintf("%s; cannot create pods: %s", msg, strings.Join(stuck, "; "))
		}
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "Provisioning", msg)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Every claim is Ready, which is a statement about Helm, not about the
	// app. The workloads were asked above — see app_workload_health.go for
	// what a Ready claim is worth on its own.
	if len(stuck) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "WorkloadCannotStart",
			fmt.Sprintf("Installed, but cannot create pods: %s", strings.Join(stuck, "; ")))
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "Provisioned", "Every installed app's component is Ready")
	return ctrl.Result{}, nil
}

// seedAppPrerequisites writes what the app Composition needs to already exist.
//
// One seam, deliberately, for the two remaining writes: the OpenBao entries the
// claim's ExternalSecret resolves, and the LiteLLM virtual key an LLM-consuming
// app authenticates with. Neither is something a Composition can do today —
// both mint or register a credential — so both are still the operator's.
//
// Grouped rather than left inline so the extraction is mechanical when there is
// somewhere to move them to, and so the loop that calls it reads as what it
// otherwise is: status aggregation.
func (r *TenantReconciler) seedAppPrerequisites(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.ComponentProfile) error {
	if err := r.seedAppSecrets(ctx, tenant, appName, profile); err != nil {
		return fmt.Errorf("seed app-secrets for %s: %w", appName, err)
	}
	if err := r.injectLLMCredentials(ctx, tenant, appName, profile); err != nil {
		return fmt.Errorf("inject llm credentials for %s: %w", appName, err)
	}
	return nil
}

// seedAppSecrets writes each AppProfile.spec.appSecrets entry into OpenBao at
// …/internal/{name} with key "value". No-op when Seeder is nil or the profile
// declares no app-secrets. Repeated calls are idempotent.
func (r *TenantReconciler) seedAppSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.ComponentProfile) error {
	if r.Seeder == nil || len(profile.GeneratedSecrets()) == 0 {
		return nil
	}
	for _, s := range profile.GeneratedSecrets() {
		if s.Name == "" {
			continue
		}
		if _, err := r.Seeder.SeedAppSecret(ctx, tenant.Name, appName, s.Name); err != nil {
			return err
		}
	}
	for _, sidecar := range profile.Spec.Extensions {
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

// appComponentReady reports whether an installed app's Component is Ready,
// which is the Component reconciler's statement that it is delivered and
// routed. A Component that does not exist yet is not ready.
func (r *TenantReconciler) appComponentReady(ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName string) (bool, error) {
	comp := &gentianov1alpha1.Component{}
	err := r.Get(ctx, types.NamespacedName{Name: profileName, Namespace: tenantNamespaceName(tenant)}, comp)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return meta.IsStatusConditionTrue(comp.Status.Conditions, "Ready"), nil
}

// injectLLMCredentials creates a secret inside the tenant namespace containing the OpenAI API
// endpoints and virtual key configured by the platform, and registers that same virtual key
// with LiteLLM itself (idempotent — see ensureLiteLLMVirtualKey) so it is actually usable
// rather than a string the proxy has never heard of.
func (r *TenantReconciler) injectLLMCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.ComponentProfile) error {
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
		for _, dsk := range profile.DerivedSecrets() {
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
// (scripts/bootstrap/seed-openbao.sh → ExternalSecret llm-sensitive-values in
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

// deleteAppDeployment removes the tenant's workloads: every Component in its
// namespace, and with each the App claim and the Helm release it owns. It
// reports errDeleteJobPending until none is left.
//
// This was a no-op that returned nil, from when the tenant's Composition
// owned the App claims. Components own them now, and nothing here removed a
// Component: they went when the namespace was deleted, or by garbage
// collection after the Tenant was gone -- after the stores, that is, so a
// tenant's apps were still running while their databases were dropped under
// them, and with deletionPolicy Retain they kept running until the Tenant
// object had disappeared. The workloads go first now, which is where the
// teardown order has them, and the deletion does not move on to the stores
// while a release is still being uninstalled: a Component is not gone before
// its release is (its finalizer sees to that).
func (r *TenantReconciler) deleteAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components, client.InNamespace(tenantNamespaceName(tenant))); err != nil {
		return fmt.Errorf("list the components of tenant %s: %w", tenant.Name, err)
	}
	if len(components.Items) == 0 {
		return nil
	}
	for i := range components.Items {
		comp := &components.Items[i]
		if comp.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, comp); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove the %s component: %w", comp.Name, err)
		}
	}
	return errDeleteJobPending
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
