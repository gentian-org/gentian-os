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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionPolicyAccepted = "Accepted"

	// clusterBackupPolicy is the cluster-scoped policy's name. Singleton by
	// convention: a second would leave "which destination applies" answerable
	// two ways.
	clusterBackupPolicy = "default"

	// managedScheduleName is the one TenantExportSchedule this operator owns
	// per tenant. Named distinctly so a schedule an admin wrote by hand is
	// never mistaken for one derived from policy, and never deleted by it.
	managedScheduleName = "policy"

	// credentialProbePrefix names the ExternalSecret that reports whether a
	// destination's keys have been supplied. One constant rather than four
	// literals, because SetupWithManager now has to recognise the name it
	// builds elsewhere, and a prefix spelled twice is a watch that silently
	// matches nothing.
	credentialProbePrefix = "credreq-"
)

// externalSecretGVK is used unstructured rather than through ESO's Go types,
// for the reason the credential manager already records: adding external-secrets
// to go.mod for a handful of fields couples this build to an API version that
// has already moved once.
var externalSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1",
	Kind:    "ExternalSecret",
}

// BackupPolicyReconciler turns a policy into the things it implies: the
// credential its destination needs, and the resolved values an admin reads.
//
// It writes no bundles and runs no Jobs. Everything here exists so that by the
// time an export runs, the question "where does this go, and can we
// authenticate to it" has already been answered and published.
type BackupPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=gentianos.io,resources=backuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=backuppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=credentialrequirements,verbs=get;list;watch;create;update;patch;delete

func (r *BackupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("backuppolicy")

	policy := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The credential comes first: a destination whose keys are missing is a
	// policy that will fail at 03:00, and the whole point of routing it
	// through the credential manager is that the failure surfaces now.
	if err := r.ensureDestinationCredential(ctx, policy); err != nil {
		return r.failPolicy(ctx, policy, "CredentialUnavailable", err.Error())
	}

	eff, err := r.resolve(ctx, policy)
	if err != nil {
		return r.failPolicy(ctx, policy, "PolicyUnusable", err.Error())
	}

	policy.Status.EffectiveEndpoint = eff.Endpoint
	policy.Status.EffectiveBucket = eff.Bucket
	policy.Status.EffectiveSchedule = eff.Schedule
	policy.Status.CredentialRequirement = eff.CredentialName
	policy.Status.CredentialSatisfied = true
	message := "backups write to the platform's own storage"

	if eff.CredentialName != "" {
		satisfied, why := r.credentialSatisfied(ctx, eff.CredentialName)
		policy.Status.CredentialSatisfied = satisfied
		if !satisfied {
			// Not an error: the policy is correct and waiting for a human to
			// supply keys. Reported as a false condition so the console can
			// show a field to fill rather than a failure to debug.
			setPolicyCondition(policy, metav1.ConditionFalse, "CredentialUnsatisfied",
				fmt.Sprintf("supply %s in the credential manager: %s", eff.CredentialName, why))
			logger.Info("policy awaiting its credential", "policy", policy.Name, "requirement", eff.CredentialName)
			return ctrl.Result{}, r.persistPolicy(ctx, policy)
		}
		message = fmt.Sprintf("backups write to %s/%s", eff.Endpoint, eff.Bucket)
	}

	if err := r.reconcileSchedules(ctx, policy); err != nil {
		return r.failPolicy(ctx, policy, "ScheduleFailed", err.Error())
	}

	setPolicyCondition(policy, metav1.ConditionTrue, "Accepted", message)
	return ctrl.Result{}, r.persistPolicy(ctx, policy)
}

// reconcileSchedules turns the resolved schedule into TenantExportSchedules.
//
// A cluster policy fans out to every tenant, because a default nobody has to
// restate per tenant is the whole point of having one — a cluster that sets
// "nightly" and then requires a schedule written by hand for each tenant has
// only documented an intention.
func (r *BackupPolicyReconciler) reconcileSchedules(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
) error {
	if policy.Spec.Scope == "tenant" {
		tenant := &gentianov1alpha1.Tenant{}
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.Tenant}, tenant); err != nil {
			return client.IgnoreNotFound(err)
		}
		return r.ensureManagedSchedule(ctx, tenant)
	}

	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return fmt.Errorf("list tenants for schedule fan-out: %w", err)
	}
	for i := range tenants.Items {
		if tenants.Items[i].DeletionTimestamp != nil {
			continue
		}
		if err := r.ensureManagedSchedule(ctx, &tenants.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// ensureManagedSchedule creates, updates or removes the one schedule this
// operator owns for a tenant.
//
// Owned by name and label, and deliberately not the only schedule a tenant may
// have: an admin who wrote their own TenantExportSchedule meant it, and a
// policy that deleted it would be overriding a more specific instruction with
// a more general one.
func (r *BackupPolicyReconciler) ensureManagedSchedule(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
) error {
	eff, err := r.effectiveForTenant(ctx, tenant)
	if err != nil {
		return err
	}

	name := managedScheduleName
	namespace := backup.TenantNamespace(tenant.Name)
	existing := &gentianov1alpha1.TenantExportSchedule{}
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)

	if eff.Schedule == "" {
		// No schedule in force: remove the one we manage, and leave any the
		// tenant wrote alone.
		if getErr == nil {
			return r.Delete(ctx, existing)
		}
		return client.IgnoreNotFound(getErr)
	}

	retention := eff.Retention
	desired := &gentianov1alpha1.TenantExportSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: gentianov1alpha1.TenantExportScheduleSpec{
			Schedule:  eff.Schedule,
			Retention: &retention,
		},
	}

	switch {
	case apierrors.IsNotFound(getErr):
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create managed schedule for %s: %w", tenant.Name, err)
		}
		return nil
	case getErr != nil:
		return getErr
	}
	existing.Spec.Schedule = desired.Spec.Schedule
	existing.Spec.Retention = desired.Spec.Retention
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

// effectiveForTenant resolves the cluster policy against one tenant's own.
func (r *BackupPolicyReconciler) effectiveForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
) (backup.Effective, error) {
	cluster := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterBackupPolicy}, cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return backup.Effective{}, err
		}
		cluster = nil
	}
	override := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, override); err != nil {
		if !apierrors.IsNotFound(err) {
			return backup.Effective{}, err
		}
		override = nil
	}
	if override != nil && (override.Spec.Scope != "tenant" || override.Spec.Tenant != tenant.Name) {
		override = nil
	}
	return backup.ResolveEffective(tenant, cluster, override)
}

// resolve merges this policy with the cluster default, when it is not itself
// the cluster default.
func (r *BackupPolicyReconciler) resolve(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
) (backup.Effective, error) {
	if policy.Spec.Scope != "tenant" {
		return backup.ResolveEffective(&gentianov1alpha1.Tenant{}, policy, nil)
	}

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.Tenant}, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return backup.Effective{}, fmt.Errorf("tenant %q does not exist", policy.Spec.Tenant)
		}
		return backup.Effective{}, err
	}

	cluster := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterBackupPolicy}, cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return backup.Effective{}, err
		}
		cluster = nil // No cluster default is ordinary; the tenant's stands alone.
	}
	return backup.ResolveEffective(tenant, cluster, policy)
}

// ensureDestinationCredential declares what an endpoint needs, and removes the
// declaration when it no longer does.
//
// The requirement is derived from the policy rather than named by it: a policy
// that could point at any requirement would let a tenant read one belonging to
// somebody else.
func (r *BackupPolicyReconciler) ensureDestinationCredential(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
) error {
	name := backup.DestinationCredentialName(policy.Spec.Scope, policy.Spec.Tenant)
	if !policy.Spec.Destination.NeedsCredential() {
		// The platform's own storage needs no requirement, and leaving a stale
		// one behind would report an unsatisfied credential nothing consumes.
		return r.deleteDestinationCredential(ctx, name)
	}

	vaultPath := backup.DestinationVaultPath(policy.Spec.Scope, policy.Spec.Tenant)
	desired := &gentianov1alpha1.CredentialRequirement{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"gentianos.io/credential-phase": "runtime",
				"gentianos.io/credential-scope": policy.Spec.Scope,
				managedByLabel:                  managedByValue,
			},
		},
		Spec: gentianov1alpha1.CredentialRequirementSpec{
			DisplayName: "Backup Storage Keys",
			Description: fmt.Sprintf(
				"Access keys for %s, where backup bundles are written. "+
					"Issued by the storage provider; the platform never sees them until they are supplied here.",
				policy.Spec.Destination.Endpoint),
			Phase:     "runtime",
			Scope:     policy.Spec.Scope,
			Tenant:    policy.Spec.Tenant,
			VaultPath: vaultPath,
			Fields: []gentianov1alpha1.CredentialField{
				{Key: backup.DestinationAccessKeyField, Format: "string", Secret: false},
				{Key: backup.DestinationSecretKeyField, Format: "password", Secret: true},
			},
			ConsumedBy: []gentianov1alpha1.CredentialConsumer{
				{Kind: "BackupPolicy", Name: policy.Name},
			},
		},
	}

	existing := &gentianov1alpha1.CredentialRequirement{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create credential requirement %s: %w", name, err)
		}
	case err != nil:
		return err
	default:
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("update credential requirement %s: %w", name, err)
		}
	}

	if err := r.ensureProbe(ctx, name, vaultPath, policy.Spec.Scope); err != nil {
		return err
	}
	return r.ensureConsumingSecret(ctx, policy, vaultPath)
}

func (r *BackupPolicyReconciler) deleteDestinationCredential(ctx context.Context, name string) error {
	req := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Delete(ctx, req); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for _, es := range []struct{ name, namespace string }{
		{credentialProbePrefix + name, meta.OperatorNamespace},
		{name, kernelNamespace},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(externalSecretGVK)
		obj.SetName(es.name)
		obj.SetNamespace(es.namespace)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ensureProbe creates the satisfaction probe: an ExternalSecret that creates
// no Secret and exists only so ESO's Ready condition answers "has this been
// supplied". The credential manager reads exactly this object.
func (r *BackupPolicyReconciler) ensureProbe(ctx context.Context, name, vaultPath, scope string) error {
	return r.applyExternalSecret(ctx, credentialProbePrefix+name, meta.OperatorNamespace, vaultPath, "None", map[string]string{
		"gentianos.io/credential-requirement": name,
		"gentianos.io/credential-phase":       "runtime",
		"gentianos.io/credential-scope":       scope,
	})
}

// ensureConsumingSecret materialises the keys where capture Jobs run.
//
// The kernel namespace, always — including for a tenant's own destination.
// Capture Jobs run there, the tenant namespace copy is staged from it for the
// volume Jobs that must run beside their PVC, and a Secret a tenant workload
// could read would hand it the keys to its own backup storage.
func (r *BackupPolicyReconciler) ensureConsumingSecret(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
	vaultPath string,
) error {
	name := backup.DestinationSecretName(policy.Spec.Scope, policy.Spec.Tenant)
	return r.applyExternalSecret(ctx, name, kernelNamespace, vaultPath, "Owner", map[string]string{
		managedByLabel:               managedByValue,
		"gentianos.io/backup-policy": policy.Name,
	})
}

func (r *BackupPolicyReconciler) applyExternalSecret(
	ctx context.Context,
	name, namespace, vaultPath, creationPolicy string,
	labels map[string]string,
) error {
	data := []any{}
	for _, key := range []string{backup.DestinationAccessKeyField, backup.DestinationSecretKeyField} {
		data = append(data, map[string]any{
			"secretKey": key,
			"remoteRef": map[string]any{"key": vaultPath, "property": key},
		})
	}

	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": externalSecretGVK.GroupVersion().String(),
		"kind":       externalSecretGVK.Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]any{"name": "openbao", "kind": "ClusterSecretStore"},
			"target":          map[string]any{"creationPolicy": creationPolicy},
			"data":            data,
		},
	}}
	desired.SetLabels(labels)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ExternalSecret %s/%s: %w", namespace, name, err)
		}
		return nil
	case err != nil:
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		return fmt.Errorf("update ExternalSecret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// credentialSatisfied reads ESO's verdict from the probe, the same way the
// credential manager does.
func (r *BackupPolicyReconciler) credentialSatisfied(ctx context.Context, name string) (bool, string) {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	key := types.NamespacedName{Name: credentialProbePrefix + name, Namespace: meta.OperatorNamespace}
	if err := r.Get(ctx, key, es); err != nil {
		return false, "no satisfaction probe found"
	}
	conds, found, err := unstructured.NestedSlice(es.Object, "status", "conditions")
	if err != nil || !found {
		return false, "probe has not reported yet"
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		if cond["status"] == "True" {
			return true, ""
		}
		if msg, ok := cond["message"].(string); ok && msg != "" {
			return false, msg
		}
		return false, "not ready"
	}
	return false, "probe has not reported a Ready condition"
}

func (r *BackupPolicyReconciler) failPolicy(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
	reason, message string,
) (ctrl.Result, error) {
	setPolicyCondition(policy, metav1.ConditionFalse, reason, message)
	return ctrl.Result{}, r.persistPolicy(ctx, policy)
}

func (r *BackupPolicyReconciler) persistPolicy(ctx context.Context, policy *gentianov1alpha1.BackupPolicy) error {
	policy.Status.ObservedGeneration = policy.Generation
	return r.Status().Update(ctx, policy)
}

func setPolicyCondition(policy *gentianov1alpha1.BackupPolicy, status metav1.ConditionStatus, reason, message string) {
	cond := metav1.Condition{
		Type:               conditionPolicyAccepted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: policy.Generation,
	}
	for i := range policy.Status.Conditions {
		if policy.Status.Conditions[i].Type != conditionPolicyAccepted {
			continue
		}
		if policy.Status.Conditions[i].Status == status &&
			policy.Status.Conditions[i].Reason == reason &&
			policy.Status.Conditions[i].Message == message {
			return // Unchanged: keep the original transition time.
		}
		policy.Status.Conditions[i] = cond
		return
	}
	policy.Status.Conditions = append(policy.Status.Conditions, cond)
}

// isCredentialProbe keeps the watch below to the ExternalSecrets this
// controller creates. Every app's ExternalSecret would otherwise wake the
// backup policies, and there is one of those per app per tenant.
func isCredentialProbe(obj client.Object) bool {
	return obj.GetNamespace() == meta.OperatorNamespace &&
		strings.HasPrefix(obj.GetName(), credentialProbePrefix)
}

// policiesForProbe maps a satisfaction probe back to the policies waiting on it.
//
// Matched on status.CredentialRequirement, which is the same name
// credentialSatisfied looks the probe up by, so the two cannot disagree about
// which policy a probe belongs to. A policy with no requirement recorded has
// not reconciled yet and will be reconciled by its own watch; including it here
// costs one no-op reconcile and covers the ordering where the probe reports
// before the policy first records what it is waiting for.
func (r *BackupPolicyReconciler) policiesForProbe(ctx context.Context, obj client.Object) []reconcile.Request {
	name := strings.TrimPrefix(obj.GetName(), credentialProbePrefix)

	policies := &gentianov1alpha1.BackupPolicyList{}
	if err := r.List(ctx, policies); err != nil {
		// Nothing to enqueue and nowhere to return an error to. Logged rather
		// than dropped: silence here is a policy that never learns its
		// credential arrived, which is the failure this watch exists to end.
		log.FromContext(ctx).Error(err, "cannot list backup policies for a credential probe",
			"probe", obj.GetName())
		return nil
	}

	var reqs []reconcile.Request
	for i := range policies.Items {
		req := policies.Items[i].Status.CredentialRequirement
		if req == name || req == "" {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: policies.Items[i].Name},
			})
		}
	}
	return reqs
}

// SetupWithManager registers the reconciler.
//
// The watch on the probe is not decoration. A policy whose keys are missing
// reports CredentialUnsatisfied and returns without requeueing — correct, since
// it is waiting on a person rather than on time — but with nothing watching the
// probe, supplying the keys never woke it. The policy stayed unsatisfied, its
// schedule was never created, and the only ways out were editing the policy or
// restarting the operator. Observed on corp: the credential landed, ESO synced
// it, and the policy still read "supply backup-destination-corp" until it was
// nudged by hand.
func (r *BackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(externalSecretGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.BackupPolicy{}).
		Watches(probe,
			handler.EnqueueRequestsFromMapFunc(r.policiesForProbe),
			builder.WithPredicates(predicate.NewPredicateFuncs(isCredentialProbe)),
		).
		Named("backuppolicy").
		Complete(r)
}
