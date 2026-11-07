/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	"github.com/gentian-org/gentian-os/internal/credentialprobe"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

const (
	conditionPolicyAccepted = "Accepted"

	// The name lives in internal/backup, because the read API the console
	// goes through answers for the same object.
	clusterBackupPolicy = backup.ClusterPolicyName
)

// BackupPolicyReconciler turns a policy into the things it implies: the
// credential its destination needs, and the resolved values an admin reads.
//
// It writes no bundles and runs no Jobs. Everything here exists so that by the
// time an export runs, the question "where does this go, and can we
// authenticate to it" has already been answered and published.
type BackupPolicyReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
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
	// policy whose next export will fail, and the whole point of routing it
	// through the custodian is that the failure surfaces now.
	if err := r.ensureDestinationCredential(ctx, policy); err != nil {
		if apierrors.IsConflict(err) {
			// Someone wrote the same object between our read and our write.
			// That is an instruction to retry, not a policy an admin needs to
			// fix, and reporting it as one is what made Accepted flip to False
			// and back about once a second: each flip rewrote the status that
			// woke the next reconcile, which conflicted again.
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.failPolicy(ctx, policy, "CredentialUnavailable", err.Error())
	}

	eff, err := r.resolve(ctx, policy)
	if err != nil {
		return r.failPolicy(ctx, policy, "PolicyUnusable", err.Error())
	}

	policy.Status.EffectiveEndpoint = eff.Endpoint
	policy.Status.EffectiveBucket = eff.Bucket
	policy.Status.EffectiveRecipients = slices.Clone(eff.Recipients)
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
			setStatusCondition(&policy.Status.Conditions, policy.Generation, conditionPolicyAccepted, metav1.ConditionFalse, "CredentialUnsatisfied",
				fmt.Sprintf("supply %s in the custodian: %s", eff.CredentialName, why))
			logger.Info("policy awaiting its credential", "policy", policy.Name, "requirement", eff.CredentialName)
			return ctrl.Result{}, r.persistPolicy(ctx, policy)
		}
		message = fmt.Sprintf("backups write to %s/%s", eff.Endpoint, eff.Bucket)
	}

	setStatusCondition(&policy.Status.Conditions, policy.Generation, conditionPolicyAccepted, metav1.ConditionTrue, "Accepted", message)
	return ctrl.Result{}, r.persistPolicy(ctx, policy)
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
		if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
			equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			return nil
		}
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
		{credentialprobe.Prefix + name, meta.OperatorNamespace},
		{name, s3Namespace},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(secrets.ExternalSecretGVK)
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
// supplied". The custodian reads exactly this object.
func (r *BackupPolicyReconciler) ensureProbe(ctx context.Context, name, vaultPath, scope string) error {
	return applyExternalSecret(ctx, r.Client, credentialprobe.Prefix+name, meta.OperatorNamespace, vaultPath, "None", nil, map[string]string{
		"gentianos.io/credential-requirement": name,
		"gentianos.io/credential-phase":       "runtime",
		"gentianos.io/credential-scope":       scope,
	})
}

// ensureConsumingSecret materializes the keys where capture Jobs run.
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
	return applyExternalSecret(ctx, r.Client, name, s3Namespace, vaultPath, "Owner", nil, map[string]string{
		managedByLabel:               managedByValue,
		"gentianos.io/backup-policy": policy.Name,
	})
}

// applyExternalSecret creates or updates an ExternalSecret that reads a
// destination's or a source's two keys from vaultPath. owners, when given,
// make it collected with what it serves.
func applyExternalSecret(
	ctx context.Context,
	c client.Client,
	name, namespace, vaultPath, creationPolicy string,
	owners []metav1.OwnerReference,
	labels map[string]string,
) error {
	data := []any{}
	for _, key := range []string{backup.DestinationAccessKeyField, backup.DestinationSecretKeyField} {
		data = append(data, secrets.RemoteRef(key, vaultPath, key))
	}
	// The kernel's store: both ExternalSecrets are in kernel namespaces, also
	// where the path is a tenant's.
	desired := secrets.ExternalSecret(name, namespace, secrets.KernelStore, map[string]any{"creationPolicy": creationPolicy}, data)
	desired.SetLabels(labels)
	if len(owners) > 0 {
		desired.SetOwnerReferences(owners)
	}
	err := ensureExternalSecret(ctx, c, desired, func(existing *unstructured.Unstructured) bool {
		return len(owners) == 0 || equality.Semantic.DeepEqual(existing.GetOwnerReferences(), desired.GetOwnerReferences())
	})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// credentialSatisfied reads ESO's verdict from the probe, the same way the
// custodian does.
func (r *BackupPolicyReconciler) credentialSatisfied(ctx context.Context, name string) (bool, string) {
	return credentialProbeSatisfied(ctx, r.Client, name)
}

// credentialProbeSatisfied reads whether the requirement name has been
// supplied, from its probe.
func credentialProbeSatisfied(ctx context.Context, c client.Client, name string) (bool, string) {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	key := types.NamespacedName{Name: credentialprobe.Prefix + name, Namespace: meta.OperatorNamespace}
	if err := c.Get(ctx, key, es); err != nil {
		return false, "no satisfaction probe found"
	}
	ready, _, reason := credentialprobe.Ready(es)
	return ready, reason
}

func (r *BackupPolicyReconciler) failPolicy(
	ctx context.Context,
	policy *gentianov1alpha1.BackupPolicy,
	reason, message string,
) (ctrl.Result, error) {
	setStatusCondition(&policy.Status.Conditions, policy.Generation, conditionPolicyAccepted, metav1.ConditionFalse, reason, message)
	return ctrl.Result{}, r.persistPolicy(ctx, policy)
}

func (r *BackupPolicyReconciler) persistPolicy(ctx context.Context, policy *gentianov1alpha1.BackupPolicy) error {
	policy.Status.ObservedGeneration = policy.Generation
	return r.Status().Update(ctx, policy)
}

// isCredentialProbe keeps the watch below to the ExternalSecrets this
// controller creates. Every app's ExternalSecret would otherwise wake the
// backup policies, and there is one of those per app per tenant.
func isCredentialProbe(obj client.Object) bool {
	return obj.GetNamespace() == meta.OperatorNamespace &&
		strings.HasPrefix(obj.GetName(), credentialprobe.Prefix)
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
	name := strings.TrimPrefix(obj.GetName(), credentialprobe.Prefix)

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
// probe, supplying the keys never woke it: the policy stayed unsatisfied until
// it was edited or the operator restarted.
func (r *BackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(secrets.ExternalSecretGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.BackupPolicy{}).
		Watches(probe,
			handler.EnqueueRequestsFromMapFunc(r.policiesForProbe),
			builder.WithPredicates(predicate.NewPredicateFuncs(isCredentialProbe)),
		).
		Named("backuppolicy").
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.BackupPolicy{}, r))
}
