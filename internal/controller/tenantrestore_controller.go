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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// TenantRestoreReconciler puts a bundle back into a live tenant.
//
// It mirrors the export loop — one app paused, worked on, resumed, then the
// next — for the same reason, and adds a preflight that runs before anything is
// touched. Discovering half way through that a bundle is unusable would leave a
// tenant with some apps restored and some not, which is worse than either
// outcome on its own.
type TenantRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Reconciler *TenantExportReconciler
	Tenant     *TenantReconciler
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
		return ctrl.Result{}, r.resumeAll(ctx, restore, tenantName)
	}

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, restore, "TenantNotFound", fmt.Sprintf("tenant %q does not exist", tenantName))
		}
		return ctrl.Result{}, err
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

	bundle, encMode, err := r.resolveBundle(ctx, restore)
	if err != nil {
		return r.fail(ctx, restore, "BundleUnusable", err.Error())
	}
	restore.Status.Bundle = bundle

	decryption, err := r.resolveDecryption(ctx, restore, encMode)
	if err != nil {
		return r.fail(ctx, restore, "DecryptionUnavailable", err.Error())
	}

	apps, err := r.restoreAppSet(ctx, tenant, restore)
	if err != nil {
		return r.fail(ctx, restore, "NothingToRestore", err.Error())
	}
	if len(apps) == 0 {
		return r.fail(ctx, restore, "NothingToRestore",
			"no installed app matches this bundle; install the apps first, then restore")
	}

	if restore.Status.StartedAt == nil {
		restore.Status.StartedAt = ptrNow()
		restore.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
		setRestoreCondition(restore, conditionExportAccepted, metav1.ConditionTrue, "Accepted",
			fmt.Sprintf("restoring %d app(s) from %s", len(apps), bundle.Prefix))
		if err := r.persist(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
	}

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
	setRestoreCondition(restore, conditionExportComplete, metav1.ConditionTrue, "Restored",
		fmt.Sprintf("%d app(s) restored; members have no credentials until they are sent a password reset", len(apps)))
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

	units := r.restoreUnits(tenant, appName, profile, restore, decryption)
	for _, unit := range units {
		if unit.Kind == "volume" {
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

// restoreUnits enumerates what to put back for one app, from the same profile
// declarations the capture was driven by.
func (r *TenantRestoreReconciler) restoreUnits(
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.AppProfile,
	restore *gentianov1alpha1.TenantRestore,
	d backup.Decryption,
) []captureUnit {
	stores := backup.ProfileStores(profile)
	spec := profileBackupSpec(profile)
	params := r.jobParams(tenant, appName, restore)

	var units []captureUnit
	switch stores.Database {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		db := backup.DatabaseName(tenant, appName)
		p := params
		p.Name = exportJobName(restore.Name, appName, "pgr")
		units = append(units, captureUnit{
			Kind: "postgres", Name: db, JobName: p.Name,
			Job: backup.PostgresRestoreJob(p, d, db),
		})
	case gentianov1alpha1.DatabaseEngineMariaDB:
		db := backup.DatabaseName(tenant, appName)
		p := params
		p.Name = exportJobName(restore.Name, appName, "myr")
		units = append(units, captureUnit{
			Kind: "mariadb", Name: db, JobName: p.Name,
			Job: backup.MariaDBRestoreJob(p, d, db),
		})
	}

	if stores.S3 {
		bucket := backup.S3Bucket(tenant, appName)
		p := params
		p.Name = exportJobName(restore.Name, appName, "s3r")
		units = append(units, captureUnit{
			Kind: "s3", Name: bucket, JobName: p.Name,
			Job: backup.S3RestoreJob(p, d, bucket),
		})
	}

	// Volume Jobs run in the tenant namespace — the PVC is only mountable
	// there. MinIO credentials come from the staged copy; the decryption key
	// is read from its original Secret, which the spec already requires to be
	// in the tenant namespace.
	volParams := params
	volParams.Namespace = backup.TenantNamespace(tenant.Name)
	volParams.UploadCredentialsSecret = restoreVolumeSecretName(restore.Name)
	volD := d
	if dec := restore.Spec.Decryption; dec != nil {
		if d.Mode == gentianov1alpha1.ExportEncryptionPassphrase && dec.PassphraseSecretRef != nil {
			volD.SecretName = dec.PassphraseSecretRef.Name
		} else if dec.IdentitySecretRef != nil {
			volD.SecretName = dec.IdentitySecretRef.Name
		}
	}
	for i, claim := range r.Reconciler.appVolumes(tenant.Name, appName, profile, spec) {
		p := volParams
		p.Name = exportJobName(restore.Name, appName, fmt.Sprintf("vr%d", i))
		units = append(units, captureUnit{
			Kind: "volume", Name: claim, JobName: p.Name,
			Job: backup.VolumeRestoreJob(p, volD, claim),
		})
	}
	return units
}

func (r *TenantRestoreReconciler) restoreTenantWide(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	d backup.Decryption,
) (bool, error) {
	params := r.jobParams(tenant, backupTenantComponent, restore)

	realmParams := params
	realmParams.Name = exportJobName(restore.Name, backupTenantComponent, "realmr")
	realm := keycloakRealmName(tenant)

	shellParams := params
	shellParams.Name = exportJobName(restore.Name, backupTenantComponent, "shellr")
	shellDB := databaseName(tenant, portalShellAppName)

	units := []captureUnit{
		{Kind: "identity", Name: realm, JobName: realmParams.Name, Job: backup.RealmImportJob(realmParams, d, realm)},
		{Kind: "postgres", Name: shellDB, JobName: shellParams.Name, Job: backup.PostgresRestoreJob(shellParams, d, shellDB)},
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
	return backup.JobParams{
		Namespace:    kernelNamespace,
		Tenant:       tenant.Name,
		App:          appName,
		Export:       restore.Name,
		Bucket:       bundle.Bucket,
		Prefix:       bundle.Prefix,
		ScratchLimit: exportScratchLimit,
		BackoffLimit: exportCaptureBackoffLimit,
	}
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
	// A bundle named directly carries no status to read, so the mode is taken
	// from whichever decryption key was supplied.
	mode := gentianov1alpha1.ExportEncryptionRecipient
	if restore.Spec.Decryption != nil && restore.Spec.Decryption.PassphraseSecretRef != nil {
		mode = gentianov1alpha1.ExportEncryptionPassphrase
	}
	return restore.Spec.Bundle, mode, nil
}

// restoreAppSet returns the installed apps this restore covers.
func (r *TenantRestoreReconciler) restoreAppSet(
	_ context.Context,
	tenant *gentianov1alpha1.Tenant,
	restore *gentianov1alpha1.TenantRestore,
) ([]string, error) {
	installed := make([]string, 0, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		if app.Profile != "" {
			installed = append(installed, app.Profile)
		}
	}
	if len(restore.Spec.Apps) == 0 {
		return installed, nil
	}
	wanted := make(map[string]struct{}, len(restore.Spec.Apps))
	for _, name := range restore.Spec.Apps {
		wanted[name] = struct{}{}
	}
	var selected []string
	for _, name := range installed {
		if _, ok := wanted[name]; ok {
			selected = append(selected, name)
		}
	}
	return selected, nil
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
	kernelErr := discardStagedSecret(ctx, r.Client, stagedDecryptionSecretName(restore.Name), kernelNamespace)
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
			Namespace: kernelNamespace,
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
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing)
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
		Complete(r)
}
