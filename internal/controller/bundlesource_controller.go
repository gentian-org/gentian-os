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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	gentianmeta "github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// BundleSourceReconciler turns a declared source into what reading it needs:
// the credential requirement the custodian fills, its satisfaction probe, and
// the Secret the keys are materialised into beside the object store, where
// the Jobs that read a source run. The same three a backup destination is
// given (BackupPolicyReconciler.ensureDestinationCredential), at the source's
// own names, each owned by the source and collected with it.
type BundleSourceReconciler struct {
	Definitions *crdcheck.Holder
	client.Client
	Scheme *runtime.Scheme
}

const conditionSourceReady = "Ready"

// +kubebuilder:rbac:groups=gentianos.io,resources=bundlesources,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=bundlesources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=credentialrequirements,verbs=get;list;watch;create;update;patch;delete

func (r *BundleSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	source := &gentianov1alpha1.BundleSource{}
	if err := r.Get(ctx, req.NamespacedName, source); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !source.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	name := backup.SourceCredentialName(source.Name)
	if err := r.ensureCredential(ctx, source); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, r.report(ctx, source, metav1.ConditionFalse, "CredentialUnavailable", err.Error())
	}
	source.Status.CredentialRequirement = name
	satisfied, why := credentialProbeSatisfied(ctx, r.Client, name)
	source.Status.CredentialSatisfied = satisfied
	if !satisfied {
		return ctrl.Result{}, r.report(ctx, source, metav1.ConditionFalse, "CredentialUnsatisfied",
			fmt.Sprintf("supply %s in the custodian: %s", name, why))
	}
	return ctrl.Result{}, r.report(ctx, source, metav1.ConditionTrue, "Ready",
		fmt.Sprintf("bundles are read from %s/%s", source.Spec.Endpoint, source.Spec.Bucket))
}

func (r *BundleSourceReconciler) ensureCredential(ctx context.Context, source *gentianov1alpha1.BundleSource) error {
	name := backup.SourceCredentialName(source.Name)
	vaultPath := backup.SourceVaultPath(source.Name)
	owner := metav1.NewControllerRef(source, gentianov1alpha1.GroupVersion.WithKind("BundleSource"))
	desired := &gentianov1alpha1.CredentialRequirement{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"gentianos.io/credential-phase": "runtime",
				"gentianos.io/credential-scope": "cluster",
				managedByLabel:                  managedByValue,
			},
		},
		Spec: gentianov1alpha1.CredentialRequirementSpec{
			DisplayName: "Bundle Source Keys: " + source.Name,
			Description: fmt.Sprintf(
				"Access keys for %s, bucket %s, where bundles are read from to import a tenant. "+
					"Issued by the storage provider; reading is all they need to allow.",
				source.Spec.Endpoint, source.Spec.Bucket),
			Phase:     "runtime",
			Scope:     "cluster",
			VaultPath: vaultPath,
			Fields: []gentianov1alpha1.CredentialField{
				{Key: backup.DestinationAccessKeyField, Format: "string", Secret: false},
				{Key: backup.DestinationSecretKeyField, Format: "password", Secret: true},
			},
			ConsumedBy: []gentianov1alpha1.CredentialConsumer{
				{Kind: "BundleSource", Name: source.Name},
			},
		},
	}
	if err := controllerutil.SetControllerReference(source, desired, r.Scheme); err != nil {
		return err
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
		if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) ||
			!equality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
			!equality.Semantic.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
			existing.Spec = desired.Spec
			existing.Labels = desired.Labels
			existing.OwnerReferences = desired.OwnerReferences
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("update credential requirement %s: %w", name, err)
			}
		}
	}

	owners := []metav1.OwnerReference{*owner}
	if err := applyExternalSecret(ctx, r.Client, credentialProbePrefix+name, gentianmeta.OperatorNamespace, vaultPath, "None", owners,
		map[string]string{
			"gentianos.io/credential-requirement": name,
			"gentianos.io/credential-phase":       "runtime",
			"gentianos.io/credential-scope":       "cluster",
		}); err != nil {
		return err
	}
	return applyExternalSecret(ctx, r.Client, name, s3Namespace, vaultPath, "Owner", owners, map[string]string{
		managedByLabel:               managedByValue,
		"gentianos.io/bundle-source": source.Name,
	})
}

func (r *BundleSourceReconciler) report(ctx context.Context, source *gentianov1alpha1.BundleSource,
	status metav1.ConditionStatus, reason, message string) error {
	before := source.Status.DeepCopy()
	meta.SetStatusCondition(&source.Status.Conditions, metav1.Condition{
		Type: conditionSourceReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: source.Generation,
	})
	source.Status.ObservedGeneration = source.Generation
	if equality.Semantic.DeepEqual(before, &source.Status) {
		return nil
	}
	return r.Status().Update(ctx, source)
}

// sourceForProbe maps a satisfaction probe back to the source it reports on.
func sourceForProbe(_ context.Context, obj client.Object) []reconcile.Request {
	name := strings.TrimPrefix(obj.GetName(), credentialProbePrefix+backup.SourceCredentialName(""))
	if name == "" || name == obj.GetName() {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

// SetupWithManager registers the reconciler. The probe is watched for the
// reason BackupPolicyReconciler.SetupWithManager gives: supplying the keys
// must wake the source that waits for them.
func (r *BundleSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(externalSecretGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.BundleSource{}).
		Watches(probe,
			handler.EnqueueRequestsFromMapFunc(sourceForProbe),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return isCredentialProbe(obj) &&
					strings.HasPrefix(obj.GetName(), credentialProbePrefix+backup.SourceCredentialName(""))
			})),
		).
		Named("bundlesource").
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.BundleSource{}, r))
}
