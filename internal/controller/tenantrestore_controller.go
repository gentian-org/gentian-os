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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// TenantRestoreReconciler puts a bundle back into a live tenant.
//
// It mirrors the export loop — one app paused, worked on, resumed, then the
// next — for the same reason, and adds a preflight that runs before anything is
// touched. Discovering half way through that a bundle is unusable would leave a
// tenant with some apps restored and some not, which is worse than either
// outcome on its own.
type TenantRestoreReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	Scheme *runtime.Scheme

	Reconciler *TenantExportReconciler
	Tenant     *TenantReconciler

	// Bundles reads a bundle's manifest, which is what says what a restore
	// puts back, and removes an uploaded bundle once a restore of it has run.
	// Required: a restore that cannot read the manifest is refused, never
	// run on a guess.
	Bundles BundleReader
}

// BundleReader is the object store as a restore uses it. *bundlestore.Store
// is one.
type BundleReader interface {
	Manifest(ctx context.Context, ref gentianov1alpha1.BundleRef, key bundlestore.Key) (*backup.Manifest, error)
	RemoveImported(ctx context.Context, ref gentianov1alpha1.BundleRef) error
}

// +kubebuilder:rbac:groups=gentianos.io,resources=tenantrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

func (r *TenantRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("tenantrestore")

	restore := &gentianov1alpha1.TenantRestore{}
	if err := r.Get(ctx, req.NamespacedName, restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tenantName := tenantNameFromNamespace(restore.Namespace)
	if tenantName == "" {
		return r.fail(ctx, restore, "NotATenantNamespace",
			fmt.Sprintf("namespace %q is not a tenant namespace", restore.Namespace))
	}

	// Resume before anything else, exactly as export does: an app left paused
	// after a crash is an outage nothing else records.
	if restore.IsTerminal() {
		if err := r.resumeAll(ctx, restore, tenantName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.removeImportedBundle(ctx, restore)
	}

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, restore, "TenantNotFound", fmt.Sprintf("tenant %q does not exist", tenantName))
		}
		return ctrl.Result{}, err
	}
	if ns := backup.TenantNamespace(tenant); ns != restore.Namespace {
		return r.fail(ctx, restore, "NotATenantNamespace",
			fmt.Sprintf("tenant %q runs in namespace %q, not in %q where this restore was created",
				tenantName, ns, restore.Namespace))
	}

	// Preflight. Everything here is a reason not to start, checked while the
	// tenant is still untouched.
	if restore.Spec.ConfirmTenant != tenantName {
		return r.fail(ctx, restore, "NotConfirmed",
			fmt.Sprintf("spec.confirmTenant is %q but this restore targets tenant %q; "+
				"a restore replaces live data and will not run without a matching confirmation",
				restore.Spec.ConfirmTenant, tenantName))
	}

	if busy, holder, err := r.tenantBusy(ctx, restore); err != nil {
		return ctrl.Result{}, err
	} else if busy {
		setRestoreCondition(restore, conditionExportAccepted, metav1.ConditionFalse, "Busy",
			fmt.Sprintf("waiting for %s to finish", holder))
		restore.Status.Phase = gentianov1alpha1.TenantExportPhasePending
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
	}

	ref, encMode, err := r.resolveBundle(ctx, restore)
	if err != nil {
		return r.fail(ctx, restore, "BundleUnusable", err.Error())
	}
	restore.Status.Bundle = ref

	decryption, err := r.resolveDecryption(ctx, restore, encMode)
	if err != nil {
		return r.fail(ctx, restore, "DecryptionUnavailable", err.Error())
	}

	// The plan, made once and before anything is changed: what the bundle's
	// manifest says it holds, checked against the tenant as it stands. From
	// here on the restore works from what it recorded, so the manifest is
	// read once and a tenant that changes under a running restore does not
	// change what the restore does.
	if restore.Status.StartedAt == nil {
		plan, err := r.plan(ctx, tenant, restore, ref)
		if err != nil {
			var refused *errRestoreRefused
			if errors.As(err, &refused) {
				return r.fail(ctx, restore, refused.reason, refused.message)
			}
			return ctrl.Result{}, err
		}
		recordPlan(restore, plan)
		restore.Status.StartedAt = ptrNow()
		restore.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
		accepted := fmt.Sprintf("restoring %d app(s) from %s", len(plan.apps), ref.Prefix)
		if len(plan.notRestored) > 0 {
			accepted += "; not restoring " + omissionsText(plan.notRestored)
		}
		setRestoreCondition(restore, conditionExportAccepted, metav1.ConditionTrue, "Accepted", accepted)
		if err := r.persist(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
	}
	apps := plannedApps(restore)

	current := nextPendingApp(restore.Status.Apps, apps)
	if err := r.resumeStale(ctx, restore, tenantName, current); err != nil {
		return ctrl.Result{}, err
	}

	if current != "" {
		return r.restoreApp(ctx, restore, tenant, current, decryption, logger)
	}

	// The realm last: bringing identity back while apps were still being
	// written to would let members sign in to half-restored data.
	if done, err := r.restoreTenantWide(ctx, restore, tenant, decryption); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
	}

	// The staged key material has done its work; keeping it would quietly
	// defeat the escrow the identity secret is supposed to live in.
	_ = r.discardStagedRestoreSecrets(ctx, restore)
	restore.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	restore.Status.CompletedAt = ptrNow()
	restore.Status.PasswordResetRequired = true
	// Complete only when nothing the bundle holds was left out. What was
	// left out was decided, and written down, before the first thing was
	// changed; the result repeats it so that nobody reads "Ready" as "all of
	// it".
	complete := len(restore.Status.NotRestored) == 0
	restore.Status.Complete = &complete
	reason := "Restored"
	message := fmt.Sprintf("%d app(s) restored; members have no credentials until they are sent a password reset", len(apps))
	if !complete {
		reason = "PartiallyRestored"
		message = fmt.Sprintf("%d app(s) restored and %d not: %s. Members have no credentials until they are sent a password reset",
			len(apps), len(restore.Status.NotRestored), omissionsText(restore.Status.NotRestored))
	}
	setRestoreCondition(restore, conditionExportComplete, metav1.ConditionTrue, reason, message)
	return ctrl.Result{}, r.persist(ctx, restore)
}

// restoreApp pauses one app, loads its stores, runs its hooks and resumes it.
func (r *TenantRestoreReconciler) restoreApp(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	decryption backup.Decryption,
	logger logr.Logger,
) (ctrl.Result, error) {
	profile, err := resolveProfile(ctx, r.Client, tenant, appName)
	if err != nil {
		return ctrl.Result{}, err
	}
	spec := profileBackupSpec(profile)
	entry := appStatus(&restore.Status.Apps, appName)

	if entry.QuiesceStart == nil {
		mode, qErr := r.Tenant.quiesceApp(ctx, tenant.Name, appName, spec)
		if qErr != nil {
			return r.failApp(ctx, restore, tenant, appName, spec, fmt.Sprintf("pause failed: %v", qErr))
		}
		entry.QuiesceStart = ptrNow()
		entry.QuiesceMode = string(mode)
		entry.Phase = gentianov1alpha1.TenantExportPhaseRunning
		entry.Message = fmt.Sprintf("paused (%s)", mode)
		markQuiesced(&restore.Status.Quiesced, appName)
		logger.Info("paused app for restore", "app", appName, "mode", mode)
		if err := r.persist(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
	}

	units, err := r.restoreUnits(ctx, tenant, appName, restore, decryption)
	if err != nil {
		// Worse here than on the export side: silently resolving no claims put
		// the database back without the files it references and resumed the app
		// on top of the mismatch, reporting Ready.
		return r.failApp(ctx, restore, tenant, appName, spec,
			fmt.Sprintf("enumerate what to restore: %v", err))
	}
	for _, unit := range units {
		if unit.Kind == bundle.ArtefactVolume {
			if err := r.ensureRestoreVolumeSecret(ctx, restore); err != nil {
				return ctrl.Result{}, fmt.Errorf("stage volume credentials: %w", err)
			}
			break
		}
	}
	allDone := true
	var pending []string
	for _, unit := range units {
		done, err := r.ensureRestoreJob(ctx, restore, unit)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			allDone = false
			pending = append(pending, unit.JobName)
		}
	}

	if !allDone {
		if entry.Attempts > exportMaxAttempts {
			return r.failApp(ctx, restore, tenant, appName, spec,
				fmt.Sprintf("restore did not succeed after %d attempts (waiting on %s)",
					entry.Attempts, strings.Join(pending, ", ")))
		}
		entry.Message = fmt.Sprintf("restoring; waiting on %s", strings.Join(pending, ", "))
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
	}

	used := gentianov1alpha1.BackupQuiesceMode(entry.QuiesceMode)
	if used == "" {
		used = quiesceModeFromMessage(entry.Message)
	}

	// A post-restore hook needs a pod to exec into, and a scale-down quiesce
	// leaves none — so for every app without a working maintenance command the
	// hooks could not run at all, and the restore failed reporting a missing
	// pod after having written the data correctly. Those apps are resumed
	// first and their hooks run once a pod answers.
	//
	// Command-mode quiesce keeps its pod throughout, so there the hooks still
	// run while users are held out, which is where they belong: the window
	// between "data replaced" and "app serving it" is the only one in which
	// "re-read what changed underneath you" means anything.
	if used == gentianov1alpha1.BackupQuiesceScaleDown && restoreHooksNeedPod(spec) {
		if entry.QuiesceEnd == nil {
			if err := r.Tenant.unquiesceApp(ctx, tenant.Name, appName, spec, used); err != nil {
				return ctrl.Result{}, fmt.Errorf("resume %s before hooks: %w", appName, err)
			}
			entry.QuiesceEnd = ptrNow()
			unmarkQuiesced(&restore.Status.Quiesced, appName)
			entry.Message = "resumed; waiting for a pod to run post-restore hooks"
			return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
		}
		if _, err := r.Tenant.runningPodForApp(ctx, tenant.Name, appName, spec.QuiesceContainer()); err != nil {
			// Bounded by the clock rather than by an attempt count: the count
			// already carries the capture Jobs' failures, and a pod that has
			// not appeared in this long is not going to.
			if time.Since(entry.QuiesceEnd.Time) > restoreHookPodTimeout {
				return r.failApp(ctx, restore, tenant, appName, spec,
					fmt.Sprintf("post-restore hooks: %v", err))
			}
			return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
		}
	}

	if err := r.runRestoreHooks(ctx, tenant.Name, appName, spec, entry); err != nil {
		return r.failApp(ctx, restore, tenant, appName, spec, err.Error())
	}

	if entry.QuiesceEnd == nil {
		if err := r.Tenant.unquiesceApp(ctx, tenant.Name, appName, spec, used); err != nil {
			return ctrl.Result{}, fmt.Errorf("resume %s: %w", appName, err)
		}
		entry.QuiesceEnd = ptrNow()
		unmarkQuiesced(&restore.Status.Quiesced, appName)
	}

	// Roll the app, but only where the quiesce did not already do it for us.
	//
	// A scale-down quiesce ends by scaling back up, so the pods serving the
	// restored data are new ones and there is nothing stale left to clear. A
	// command-mode quiesce keeps the same pod throughout, and that pod has now
	// had its database, bucket and volumes replaced underneath it while holding
	// caches and subPath mounts from before the restore.
	//
	// After the resume, not before: the hooks above need a pod to exec into,
	// and a pod restarted while maintenance mode was still on would come back
	// still holding users out.
	if used != gentianov1alpha1.BackupQuiesceScaleDown {
		if err := r.Tenant.restartAppWorkloads(ctx, tenant.Name, appName); err != nil {
			return ctrl.Result{}, fmt.Errorf("restart %s after restore: %w", appName, err)
		}
		logger.Info("rolled app after restore", "app", appName)
	}

	entry.Phase = gentianov1alpha1.TenantExportPhaseReady
	entry.Message = ""
	logger.Info("restored app", "app", appName)
	if err := r.persist(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// restoreHookPodTimeout bounds the wait for an app's pod to come back before
// its post-restore hooks can run.
const restoreHookPodTimeout = 5 * time.Minute

// restoreHooksNeedPod reports whether this profile has anything to exec after a
// restore. Nothing to run means nothing to wait for a pod for.
func restoreHooksNeedPod(spec *gentianov1alpha1.BackupSpec) bool {
	post, verify := spec.RestoreCommands()
	return len(post) > 0 || len(verify) > 0
}

// runRestoreHooks runs the profile's post-restore commands and its verification.
//
// Verification is not optional decoration: without it a restore can only report
// that bytes were written, not that the app can read them, and "the backup
// restored fine" is exactly the sentence that precedes discovering otherwise.
func (r *TenantRestoreReconciler) runRestoreHooks(
	ctx context.Context,
	tenantName, appName string,
	spec *gentianov1alpha1.BackupSpec,
	entry *gentianov1alpha1.AppExportStatus,
) error {
	post, verify := spec.RestoreCommands()
	if len(post) == 0 && len(verify) == 0 {
		return nil
	}
	if r.Tenant.Exec == nil {
		// Recorded rather than silently skipped: an app whose post-restore
		// hooks never ran may serve stale caches over restored data.
		entry.Message = "post-restore hooks skipped: exec is not configured"
		return nil
	}

	for _, argv := range post {
		if _, err := r.Tenant.execAppCommand(ctx, tenantName, appName, spec, argv); err != nil {
			return fmt.Errorf("post-restore hook %v failed: %w", argv, err)
		}
	}
	if len(verify) > 0 {
		if _, err := r.Tenant.execAppCommand(ctx, tenantName, appName, spec, verify); err != nil {
			return fmt.Errorf("restore verification failed — the data is in place but the app cannot read it: %w", err)
		}
	}
	return nil
}

// restoreUnits builds the Jobs that put one app back, from the plan the
// restore recorded for it: each artefact the manifest names, into the store
// of that kind the installed app has.
func (r *TenantRestoreReconciler) restoreUnits(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	restore *gentianov1alpha1.TenantRestore,
	d backup.Decryption,
) ([]captureUnit, error) {
	entry := appStatus(&restore.Status.Apps, appName)
	params := r.jobParams(tenant, appName, restore)

	// Volume Jobs run in the tenant namespace — the PVC is only mountable
	// there. MinIO credentials come from the staged copy; the decryption key
	// is read from its original Secret, which the spec already requires to be
	// in the tenant namespace.
	volParams := params
	volParams.Namespace = backup.TenantNamespace(tenant)
	volParams.UploadCredentialsSecret = restoreVolumeSecretName(restore.Name)
	volD := d
	if dec := restore.Spec.Decryption; dec != nil {
		if d.Mode == gentianov1alpha1.ExportEncryptionPassphrase && dec.PassphraseSecretRef != nil {
			volD.SecretName = dec.PassphraseSecretRef.Name
		} else if dec.IdentitySecretRef != nil {
			volD.SecretName = dec.IdentitySecretRef.Name
		}
	}

	var units []captureUnit
	volumes := 0
	for _, a := range entry.Artefacts {
		p := params
		switch a.Kind {
		case bundle.ArtefactPostgres:
			p.Name = exportJobName(restore.Name, appName, "pgr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresRestoreJob(p, d, a.Path, a.Target)})
		case bundle.ArtefactPostgresOwned:
			p.Name = exportJobName(restore.Name, appName, "pgor")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresOwnedRestoreJob(p, d, a.Path, a.Target)})
		case bundle.ArtefactMariaDB:
			p.Name = exportJobName(restore.Name, appName, "myr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.MariaDBRestoreJob(p, d, a.Path, a.Target)})
		case bundle.ArtefactS3:
			// The bucket's user and policy, by the code install provisions
			// them with and the key pair the vault holds for the app.
			provision, err := r.Tenant.objectStorageProvisioner(ctx, tenant, appName, "provision-bucket")
			if err != nil {
				return nil, err
			}
			p.Name = exportJobName(restore.Name, appName, "s3r")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.S3RestoreJob(p, d, a.Path, a.Target, provision)})
		case bundle.ArtefactVolume:
			p = volParams
			p.Name = exportJobName(restore.Name, appName, fmt.Sprintf("vr%d", volumes))
			volumes++
			// Same constraint as capture, and worse here: this mounts the claim
			// read-write. An app paused by maintenance mode keeps its volume, so
			// without this the restore waits on a Multi-Attach that never resolves.
			p.Node = r.Reconciler.nodeHoldingClaim(ctx, p.Namespace, a.Target)
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.VolumeRestoreJob(p, volD, a.Path, a.Target)})
		default:
			// The plan refuses a kind it does not know; reaching this is a
			// plan written by something else.
			return nil, fmt.Errorf("the plan for %s names an artefact of kind %q, which cannot be restored", appName, a.Kind)
		}
	}
	return units, nil
}

func (r *TenantRestoreReconciler) restoreTenantWide(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	d backup.Decryption,
) (bool, error) {
	params := r.jobParams(tenant, backupTenantComponent, restore)
	entry := appStatus(&restore.Status.Apps, backupTenantComponent)

	var units []captureUnit
	for _, a := range entry.Artefacts {
		switch a.Kind {
		case bundle.ArtefactIdentity:
			p := params
			p.Name = exportJobName(restore.Name, backupTenantComponent, "realmr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.RealmImportJob(p, d, a.Path, a.Target)})
		case bundle.ArtefactPostgres:
			p := params
			p.Name = exportJobName(restore.Name, backupTenantComponent, "shellr")
			// Named for the shell, not for the tenant-wide component the Job is
			// labelled with -- that is the role the provisioner created alongside the
			// database.
			p.Role = backup.PostgresRole(tenant.Name, portalShellAppName)
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresRestoreJob(p, d, a.Path, a.Target)})
		}
	}

	allDone := true
	for _, unit := range units {
		done, err := r.ensureRestoreJob(ctx, restore, unit)
		if err != nil {
			return false, err
		}
		if !done {
			allDone = false
		}
	}
	if allDone {
		entry.Phase = gentianov1alpha1.TenantExportPhaseReady
		entry.Message = ""
	}
	return allDone, nil
}

func (r *TenantRestoreReconciler) ensureRestoreJob(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	unit captureUnit,
) (bool, error) {
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: unit.JobName, Namespace: unit.Job.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, unit.Job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create restore Job %s: %w", unit.JobName, err)
		}
		return false, nil
	case err != nil:
		return false, err
	}

	if jobIsComplete(existing) {
		return true, nil
	}
	if jobIsFailed(existing) {
		entry := appStatus(&restore.Status.Apps, existing.Labels[meta.AppLabel])
		entry.Attempts++
		if err := r.Delete(ctx, existing,
			client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

func (r *TenantRestoreReconciler) jobParams(
	tenant *gentianov1alpha1.Tenant,
	appName string,
	restore *gentianov1alpha1.TenantRestore,
) backup.JobParams {
	bundle := restore.Status.Bundle
	p := backup.JobParams{
		Namespace: s3Namespace,
		Tenant:    tenant.Name,
		App:       appName,
		Export:    restore.Name,
		Bucket:    bundle.Bucket,
		Prefix:    bundle.Prefix,
		// The bundle's own record of where it was written, never the policy's
		// current answer. A tenant that moved its destination last week must
		// still be able to restore what it wrote the week before.
		Endpoint:     bundle.Endpoint,
		Region:       bundle.Region,
		ScratchLimit: exportScratchLimit,
		BackoffLimit: exportCaptureBackoffLimit,
	}
	if bundle.CredentialSecret != "" {
		p.UploadCredentialsSecret = bundle.CredentialSecret
	}
	return p
}

// resolveBundle finds the bundle to restore and how it was encrypted.
func (r *TenantRestoreReconciler) resolveBundle(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
) (*gentianov1alpha1.BundleRef, gentianov1alpha1.ExportEncryptionMode, error) {
	if restore.Spec.ExportRef != "" {
		export := &gentianov1alpha1.TenantExport{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: restore.Spec.ExportRef, Namespace: restore.Namespace,
		}, export); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "", fmt.Errorf("export %q not found in this namespace", restore.Spec.ExportRef)
			}
			return nil, "", err
		}
		if export.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
			return nil, "", fmt.Errorf("export %q is %s, not Ready — an incomplete bundle cannot be restored",
				export.Name, export.Status.Phase)
		}
		if export.Status.Bundle == nil {
			return nil, "", fmt.Errorf("export %q records no bundle", export.Name)
		}
		mode := gentianov1alpha1.ExportEncryptionRecipient
		if export.Status.Encryption != nil && export.Status.Encryption.Mode != "" {
			mode = export.Status.Encryption.Mode
		}
		return export.Status.Bundle, mode, nil
	}

	if restore.Spec.Bundle == nil || restore.Spec.Bundle.Prefix == "" {
		return nil, "", fmt.Errorf("set either spec.exportRef or spec.bundle")
	}
	// A bundle named directly is named by whoever wrote the restore, and
	// its bucket and prefix are spliced into the scripts the restore Jobs
	// run with the object store's admin credential.
	if err := validBundleRef(restore.Spec.Bundle); err != nil {
		return nil, "", err
	}
	// A bundle named directly carries no status to read, so the mode is taken
	// from whichever decryption key was supplied.
	mode := gentianov1alpha1.ExportEncryptionRecipient
	if restore.Spec.Decryption != nil && restore.Spec.Decryption.PassphraseSecretRef != nil {
		mode = gentianov1alpha1.ExportEncryptionPassphrase
	}
	return restore.Spec.Bundle, mode, nil
}

var (
	bundleBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	bundlePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*/?$`)
)

// validBundleRef refuses a bucket or prefix that is not a plain name. Both
// reach a shell.
func validBundleRef(ref *gentianov1alpha1.BundleRef) error {
	if !bundleBucketPattern.MatchString(ref.Bucket) {
		return fmt.Errorf("spec.bundle.bucket %q is not a bucket name", ref.Bucket)
	}
	if !bundlePrefixPattern.MatchString(ref.Prefix) || strings.Contains(ref.Prefix, "..") {
		return fmt.Errorf("spec.bundle.prefix %q is not a plain path of letters, digits, dots, dashes and underscores", ref.Prefix)
	}
	return nil
}

// plan reads the bundle's manifest and decides, against the tenant as it
// stands, what this restore puts back and what it does not.
func (r *TenantRestoreReconciler) plan(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	restore *gentianov1alpha1.TenantRestore,
	ref *gentianov1alpha1.BundleRef,
) (*restorePlan, error) {
	if r.Bundles == nil {
		return nil, refuseRestore("BundleUnusable", "this operator cannot read bundles, so it cannot tell what this one holds; nothing was changed")
	}
	key, err := r.bundleKey(ctx, restore)
	if err != nil {
		return nil, refuseRestore("DecryptionUnavailable", "%v", err)
	}
	manifest, err := r.Bundles.Manifest(ctx, *ref, key)
	if err != nil {
		return nil, refuseRestore("BundleUnusable", "the bundle's manifest could not be read, so what the bundle holds is not known: %v. Nothing was changed", err)
	}

	profiles, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	live := func(app string) (liveApp, error) {
		now := liveApp{}
		for _, a := range tenant.Spec.Apps {
			if a.Profile == app {
				now.installed = true
			}
		}
		profile, ok := appProfileFromIndex(profiles, app)
		if !now.installed || !ok {
			return now, nil
		}
		now.profile = profile
		claims, err := r.Reconciler.appVolumes(ctx, tenant, app, profile, profileBackupSpec(profile))
		if err != nil {
			return now, fmt.Errorf("list the volume claims of %s: %w", app, err)
		}
		now.claims = claims
		return now, nil
	}
	return planRestore(manifest, tenant, restore.Spec.Apps, restore.Spec.SkipVersionCheck, live)
}

// bundleKey reads the key material the restore names, to open the manifest
// with. The same Secret the restore Jobs are handed a staged copy of.
func (r *TenantRestoreReconciler) bundleKey(ctx context.Context, restore *gentianov1alpha1.TenantRestore) (bundlestore.Key, error) {
	spec := restore.Spec.Decryption
	if spec == nil {
		return bundlestore.Key{}, fmt.Errorf("spec.decryption names no key")
	}
	read := func(ref *gentianov1alpha1.SecretKeyRef, key string) (string, error) {
		if ref.Key != "" {
			key = ref.Key
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: restore.Namespace}, secret); err != nil {
			return "", fmt.Errorf("decryption Secret %q in %s: %w", ref.Name, restore.Namespace, err)
		}
		value := string(secret.Data[key])
		if value == "" {
			return "", fmt.Errorf("decryption Secret %q has no non-empty key %q", ref.Name, key)
		}
		return value, nil
	}
	switch {
	case spec.PassphraseSecretRef != nil:
		value, err := read(spec.PassphraseSecretRef, "passphrase")
		return bundlestore.Key{Passphrase: value}, err
	case spec.IdentitySecretRef != nil:
		value, err := read(spec.IdentitySecretRef, "identity")
		return bundlestore.Key{Identity: value}, err
	}
	return bundlestore.Key{}, fmt.Errorf("spec.decryption names no key")
}

// recordPlan writes the plan into the restore's status: one entry per app
// with the artefacts it will restore, the tenant's own, what is not restored
// and why, and what no restore brings back.
func recordPlan(restore *gentianov1alpha1.TenantRestore, plan *restorePlan) {
	restore.Status.BundleSchemaVersion = plan.schemaVersion
	restore.Status.NameDerivation = plan.derivation
	restore.Status.NotRestored = plan.notRestored
	restore.Status.Notes = restoreLimits(plan.derivation)
	restore.Status.Apps = nil
	for _, app := range plan.apps {
		entry := appStatus(&restore.Status.Apps, app.name)
		entry.Artefacts = app.artefacts
		entry.Stores = artefactKinds(app.artefacts)
		// Among the result's notes, not on the app's entry: the entry's
		// message is progress and is cleared when the app is done.
		if app.note != "" {
			restore.Status.Notes = append(restore.Status.Notes, app.name+": "+app.note)
		}
	}
	wide := appStatus(&restore.Status.Apps, backupTenantComponent)
	wide.Artefacts = plan.tenantWide
	wide.Stores = artefactKinds(plan.tenantWide)
}

// plannedApps are the apps the recorded plan restores, in its order.
func plannedApps(restore *gentianov1alpha1.TenantRestore) []string {
	var apps []string
	for _, entry := range restore.Status.Apps {
		if entry.Name != backupTenantComponent {
			apps = append(apps, entry.Name)
		}
	}
	return apps
}

func artefactKinds(artefacts []gentianov1alpha1.BundleArtefact) []string {
	var kinds []string
	for _, a := range artefacts {
		if !slices.Contains(kinds, a.Kind) {
			kinds = append(kinds, a.Kind)
		}
	}
	return kinds
}

// removeImportedBundle deletes an uploaded bundle once a restore of it has
// run to its end, restored or failed. A restore that was refused before it
// started -- the wrong key, no confirmation, nothing restorable -- changed
// nothing and leaves the upload, so the request can be made again without
// uploading the bundle a second time. A bundle that is not an upload is a
// tenant's own backup and is never removed here.
func (r *TenantRestoreReconciler) removeImportedBundle(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	ref := restore.Status.Bundle
	if restore.Status.ImportRemoved || restore.Status.StartedAt == nil || !bundlestore.IsImported(ref) {
		return nil
	}
	if r.Bundles == nil {
		return nil
	}
	if err := r.Bundles.RemoveImported(ctx, *ref); err != nil {
		return fmt.Errorf("remove the uploaded bundle %s/%s: %w", ref.Bucket, ref.Prefix, err)
	}
	restore.Status.ImportRemoved = true
	return r.persist(ctx, restore)
}

// tenantBusy reports whether an export or another restore holds this tenant.
func (r *TenantRestoreReconciler) tenantBusy(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
) (bool, string, error) {
	exports := &gentianov1alpha1.TenantExportList{}
	if err := r.List(ctx, exports, client.InNamespace(restore.Namespace)); err != nil {
		return false, "", err
	}
	for i := range exports.Items {
		if !exports.Items[i].IsTerminal() && exports.Items[i].Status.StartedAt != nil {
			return true, "export " + exports.Items[i].Name, nil
		}
	}

	restores := &gentianov1alpha1.TenantRestoreList{}
	if err := r.List(ctx, restores, client.InNamespace(restore.Namespace)); err != nil {
		return false, "", err
	}
	for i := range restores.Items {
		other := &restores.Items[i]
		if other.Name == restore.Name || other.IsTerminal() {
			continue
		}
		if other.CreationTimestamp.Before(&restore.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&restore.CreationTimestamp) && other.Name < restore.Name) {
			return true, "restore " + other.Name, nil
		}
	}
	return false, "", nil
}

func (r *TenantRestoreReconciler) failApp(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	spec *gentianov1alpha1.BackupSpec,
	message string,
) (ctrl.Result, error) {
	entry := appStatus(&restore.Status.Apps, appName)
	if err := r.Tenant.unquiesceApp(ctx, tenant.Name, appName, spec, quiesceModeFromMessage(entry.Message)); err != nil {
		return ctrl.Result{}, fmt.Errorf("resume %s after failure: %w", appName, err)
	}
	unmarkQuiesced(&restore.Status.Quiesced, appName)
	entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
	entry.Message = message
	return r.fail(ctx, restore, "RestoreFailed", fmt.Sprintf("%s: %s", appName, message))
}

// stagedDecryptionSecretName names the kernel-namespace copy of the
// operator-supplied key material (see stageDecryptionSecret).
func stagedDecryptionSecretName(restoreName string) string {
	return "trs-" + restoreName + "-key"
}

// discardStagedRestoreSecrets removes both staged copies: the decryption key
// beside the kernel Jobs, and the volume credentials in the tenant namespace.
// The kernel copy leaking past the restore was a real gap — the identity is
// escrowed off-cluster precisely so the cluster does not hold it.
func (r *TenantRestoreReconciler) discardStagedRestoreSecrets(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	kernelErr := discardStagedSecret(ctx, r.Client, stagedDecryptionSecretName(restore.Name), s3Namespace)
	tenantErr := r.discardRestoreVolumeSecret(ctx, restore)
	if kernelErr != nil {
		return kernelErr
	}
	return tenantErr
}

func (r *TenantRestoreReconciler) fail(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	reason, message string,
) (ctrl.Result, error) {
	_ = r.discardStagedRestoreSecrets(ctx, restore)
	restore.Status.Phase = gentianov1alpha1.TenantExportPhaseFailed
	restore.Status.CompletedAt = ptrNow()
	// A restore that began and failed is not complete, and says so in the
	// field a reader looks at: the apps still Pending in status.apps are the
	// ones it never reached.
	if restore.Status.StartedAt != nil {
		incomplete := false
		restore.Status.Complete = &incomplete
	}
	setRestoreCondition(restore, conditionExportComplete, metav1.ConditionFalse, reason, message)
	return ctrl.Result{}, r.persist(ctx, restore)
}

func (r *TenantRestoreReconciler) resumeAll(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenantName string,
) error {
	if len(restore.Status.Quiesced) == 0 {
		return nil
	}
	for _, appName := range append([]string(nil), restore.Status.Quiesced...) {
		entry := appStatus(&restore.Status.Apps, appName)
		if err := resumeQuiescedApp(ctx, r.Client, r.Tenant,
			tenantName, appName, entry.QuiesceMode, entry.Message); err != nil {
			return fmt.Errorf("resume %s: %w", appName, err)
		}
		unmarkQuiesced(&restore.Status.Quiesced, appName)
	}
	return r.persist(ctx, restore)
}

func (r *TenantRestoreReconciler) resumeStale(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenantName, current string,
) error {
	changed := false
	for _, appName := range append([]string(nil), restore.Status.Quiesced...) {
		if appName == current {
			continue
		}
		entry := appStatus(&restore.Status.Apps, appName)
		if err := resumeQuiescedApp(ctx, r.Client, r.Tenant,
			tenantName, appName, entry.QuiesceMode, entry.Message); err != nil {
			return fmt.Errorf("resume stale %s: %w", appName, err)
		}
		unmarkQuiesced(&restore.Status.Quiesced, appName)
		changed = true
	}
	if changed {
		return r.persist(ctx, restore)
	}
	return nil
}

// resolveDecryption stages the key material the restore Jobs need.
func (r *TenantRestoreReconciler) resolveDecryption(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	mode gentianov1alpha1.ExportEncryptionMode,
) (backup.Decryption, error) {
	d := backup.Decryption{Mode: mode}
	spec := restore.Spec.Decryption
	if spec == nil {
		return d, d.Validate()
	}

	ref := spec.IdentitySecretRef
	key := "identity"
	if mode == gentianov1alpha1.ExportEncryptionPassphrase {
		ref, key = spec.PassphraseSecretRef, "passphrase"
	}
	if ref == nil {
		return d, d.Validate()
	}
	if ref.Key != "" {
		key = ref.Key
	}

	staged, err := r.stageDecryptionSecret(ctx, restore, ref.Name, key)
	if err != nil {
		return d, err
	}
	d.SecretName, d.SecretKey = staged, key
	return d, d.Validate()
}

func (r *TenantRestoreReconciler) stageDecryptionSecret(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	sourceName, key string,
) (string, error) {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: sourceName, Namespace: restore.Namespace,
	}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("decryption Secret %q not found in %s", sourceName, restore.Namespace)
		}
		return "", err
	}
	value, ok := source.Data[key]
	if !ok || len(value) == 0 {
		return "", fmt.Errorf("decryption Secret %q has no non-empty key %q", sourceName, key)
	}

	name := stagedDecryptionSecretName(restore.Name)
	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s3Namespace,
			Labels: map[string]string{
				tenantLabel:        tenantNameFromNamespace(restore.Namespace),
				managedByLabel:     managedByValue,
				backup.ExportLabel: restore.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{key: value},
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: s3Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return name, r.Create(ctx, copied)
	case err != nil:
		return "", err
	}
	existing.Data = copied.Data
	return name, r.Update(ctx, existing)
}

func (r *TenantRestoreReconciler) persist(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	restore.Status.ObservedGeneration = restore.Generation
	return r.Status().Update(ctx, restore)
}

func (r *TenantRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.TenantRestore{}).
		Named("tenantrestore").
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.TenantRestore{}, r))
}
