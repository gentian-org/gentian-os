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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

	if !restore.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, restore, tenantName)
	}

	// Resume before anything else, exactly as export does: an app left paused
	// after a crash is an outage nothing else records.
	if restore.IsTerminal() {
		if err := r.resumeAll(ctx, restore, tenantName); err != nil {
			return ctrl.Result{}, err
		}
		// Again, for a restore that reached its end with the operator stopping
		// before the copies were removed.
		if err := r.discardStagedRestoreSecrets(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.removeImportedBundle(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		// Nothing is paused and nothing is staged: the restore holds nothing
		// a deletion would have to put back.
		if controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
			controllerutil.RemoveFinalizer(restore, restoreFinalizer)
			return ctrl.Result{}, r.Update(ctx, restore)
		}
		return ctrl.Result{}, nil
	}
	// Held from before anything is paused: deleting a restore is the only way
	// to stop one, and this loop is the only thing that knows an app is paused.
	if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		controllerutil.AddFinalizer(restore, restoreFinalizer)
		if err := r.Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
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
		if appStatus(&restore.Status.Apps, current).Retained {
			return r.restoreRetained(ctx, restore, tenant, current, decryption)
		}
		return r.restoreApp(ctx, restore, tenant, current, decryption, logger)
	}

	// The realm last: bringing identity back while apps were still being
	// written to would let members sign in to half-restored data.
	if done, err := r.restoreTenantWide(ctx, restore, tenant, decryption); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		if entry := appStatus(&restore.Status.Apps, backupTenantComponent); entry.Attempts > exportMaxAttempts {
			why := fmt.Sprintf("the realm or the desktop's database did not restore after %d attempts", entry.Attempts)
			if entry.LastFailure != "" {
				why += " — " + entry.LastFailure
			}
			entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
			entry.Message = why
			return r.fail(ctx, restore, "RestoreFailed", why+". "+restoreStateText(restore, backupTenantComponent))
		}
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
	if err := r.stageFor(ctx, restore, units, decryption); err != nil {
		// The app is paused: failing is what resumes it. Nothing of it was
		// replaced by a unit that never ran.
		return r.failApp(ctx, restore, tenant, appName, spec, err.Error())
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
			why := fmt.Sprintf("restore did not succeed after %d attempts (waiting on %s)",
				entry.Attempts, strings.Join(pending, ", "))
			if entry.LastFailure != "" {
				why += " — " + entry.LastFailure
			}
			return r.failApp(ctx, restore, tenant, appName, spec, why)
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

// rightsProjectionWait bounds the wait for the operator's projection to
// attach a tenant in the rights store: longer than the interval it runs on.
const rightsProjectionWait = 15 * time.Minute

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

	// Each unit where the credential it works with is: a database unit beside
	// its database, a volume unit in the tenant's namespace -- the claim mounts
	// nowhere else -- and a bucket unit beside the object store.
	pgParams, pgD := r.placeRestoreUnit(restore, params, d, postgresNamespace)
	mariaParams, mariaD := r.placeRestoreUnit(restore, params, d, mariadbNamespace)
	volParams, volD := r.placeRestoreUnit(restore, params, d, backup.TenantNamespace(tenant))
	name := func(unit string) string { return exportJobName(tenant.Name, restore.Name, appName, unit) }

	var units []captureUnit
	volumes := 0
	for _, a := range entry.Artefacts {
		p := params
		switch a.Kind {
		case bundle.ArtefactPostgres:
			p = pgParams
			p.Name = name("pgr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresRestoreJob(p, pgD, a.Path, a.Target)})
		case bundle.ArtefactPostgresOwned:
			// The provisioned database the bundle's app had, which the names
			// in the archive are told from: the manifest names it on the
			// app's own database artefact.
			source := a.Target
			for _, sibling := range entry.Artefacts {
				if sibling.Kind == bundle.ArtefactPostgres && sibling.Name != "" {
					source = sibling.Name
				}
			}
			p = pgParams
			p.Name = name("pgor")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresOwnedRestoreJob(p, pgD, a.Path, source, a.Target)})
		case bundle.ArtefactMariaDB:
			p = mariaParams
			p.Name = name("myr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.MariaDBRestoreJob(p, mariaD, a.Path, a.Target)})
		case bundle.ArtefactMariaDBOwned:
			// a.Name is the provisioned database the archive's names begin
			// with; here they begin with a.Target.
			p = mariaParams
			p.Name = name("myor")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.MariaDBOwnedRestoreJob(p, mariaD, a.Path, a.Name, a.Target, backup.MariaDBUser(tenant.Name, appName))})
		case bundle.ArtefactS3:
			// The bucket's user and policy, by the code install provisions
			// them with and the key pair the vault holds for the app.
			provision, err := r.Tenant.objectStorageProvisioner(ctx, tenant, appName, "provision-bucket")
			if err != nil {
				return nil, err
			}
			p.Name = name("s3r")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.S3RestoreJob(p, d, a.Path, a.Target, provision)})
		case bundle.ArtefactVolume:
			p = volParams
			p.Name = name(fmt.Sprintf("vr%d", volumes))
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

// tenantWideRestoreUnits are the units that put back what is the tenant's
// and no app's, from the plan: the realm in the identity namespace, the
// desktop's database beside the tenants' PostgreSQL, each where its
// administrator Secret is.
func (r *TenantRestoreReconciler) tenantWideRestoreUnits(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	restore *gentianov1alpha1.TenantRestore,
	d backup.Decryption,
) ([]captureUnit, error) {
	params := r.jobParams(tenant, backupTenantComponent, restore)
	entry := appStatus(&restore.Status.Apps, backupTenantComponent)

	var units []captureUnit
	for _, a := range entry.Artefacts {
		switch a.Kind {
		case bundle.ArtefactIdentity:
			p, unitD := r.placeRestoreUnit(restore, params, d, identityNamespace)
			p.Name = exportJobName(tenant.Name, restore.Name, backupTenantComponent, "realmr")
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.RealmImportJob(p, unitD, a.Path, a.Target,
					backup.RealmSource{Tenant: restore.Status.SourceTenant, Realm: a.Name})})
		case bundle.ArtefactPostgres:
			_, namespace := r.Reconciler.desktopDatabase(tenant)
			p, unitD := r.placeRestoreUnit(restore, params, d, namespace)
			p.Name = exportJobName(tenant.Name, restore.Name, backupTenantComponent, "shellr")
			if r.Reconciler.desktopOnKernelStore(tenant) {
				// On the kernel's PostgreSQL, as the database's owner: there
				// is no administrator's credential beside that server.
				units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
					Job: backup.KernelDesktopRestoreJob(p, unitD, a.Path)})
				continue
			}
			// Named for the shell, not for the tenant-wide component the Job is
			// labelled with -- that is the role the provisioner created alongside the
			// database.
			p.Role = backup.PostgresRole(tenant.Name, portalShellAppName)
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.PostgresRestoreJob(p, unitD, a.Path, a.Target)})
		case bundle.ArtefactMailboxes:
			// Beside the mail server's volume, on the node it is held on.
			// Where the volume is, is asked now: the plan decided the domain.
			boxes, _, err := r.Tenant.mailboxesOf(ctx, tenant)
			if err != nil {
				return nil, err
			}
			if boxes == nil {
				return nil, fmt.Errorf("the plan puts mailboxes back into %s, and this cluster no longer keeps mailboxes for the tenant", a.Target)
			}
			p, unitD := r.placeRestoreUnit(restore, params, d, mailNamespace)
			p.Name = exportJobName(tenant.Name, restore.Name, backupTenantComponent, "mailr")
			p.Node = boxes.Node
			units = append(units, captureUnit{Kind: a.Kind, Name: a.Target, JobName: p.Name,
				Job: backup.MailboxRestoreJob(p, unitD, a.Path, boxes.Claim, a.Target)})
		default:
			// The plan lists only what it knows; reaching this is a plan
			// written by something else.
			return nil, fmt.Errorf("the plan names a tenant's artefact of kind %q, which cannot be restored", a.Kind)
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
	entry := appStatus(&restore.Status.Apps, backupTenantComponent)
	units, err := r.tenantWideRestoreUnits(ctx, tenant, restore, d)
	if err != nil {
		entry.LastFailure = err.Error()
		entry.Attempts++
		return false, nil
	}
	if err := r.stageFor(ctx, restore, units, d); err != nil {
		// Counted, like a step that failed: what cannot be staged now is not
		// staged by waiting, and the restore must end.
		entry.LastFailure = err.Error()
		entry.Attempts++
		return false, nil
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
	if !allDone {
		return false, nil
	}
	// The archived mailboxes are back below the archive; each is put on
	// record, so that it is listed and can be deleted where it is now.
	if err := r.recordArchivedMailboxes(ctx, tenant, restore); err != nil {
		entry.LastFailure = err.Error()
		entry.Attempts++
		return false, nil
	}
	// The rights last: after the realm, whose groups they name, and after
	// the operator's projection has attached the tenant -- which writes the
	// defaults a bundle may say were withdrawn.
	if r.Tenant != nil {
		done, err := applyRights(ctx, r.Tenant.Rights, r.Tenant.ClusterID, tenant.Name, restore.Status.Rights)
		if err != nil {
			entry.LastFailure = err.Error()
			entry.Attempts++
			return false, nil
		}
		if !done {
			// Not a failure yet: the projection runs when a tenant changes
			// and on its own interval, so it is waited for by the clock.
			entry.Message = "waiting for the operator to attach the tenant in the rights store, to put its rights back"
			rights := restore.Status.Rights
			if rights.WaitingSince == nil {
				rights.WaitingSince = ptrNow()
			} else if time.Since(rights.WaitingSince.Time) > rightsProjectionWait {
				entry.LastFailure = fmt.Sprintf("the operator did not attach the tenant in the rights store within %s, so its rights could not be put back", rightsProjectionWait)
				entry.Attempts = exportMaxAttempts + 1
			}
			return false, nil
		}
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseReady
	entry.Message = ""
	return true, nil
}

// recordArchivedMailboxes writes a MailboxRemoval for each archived mailbox
// the restore put back, unless the tenant has one for that archive already.
// The record says it was restored: the operator moves and removes nothing
// for it, and only reports what it states.
func (r *TenantRestoreReconciler) recordArchivedMailboxes(ctx context.Context, tenant *gentianov1alpha1.Tenant, restore *gentianov1alpha1.TenantRestore) error {
	if len(restore.Status.ArchivedMailboxes) == 0 {
		return nil
	}
	existing := &gentianov1alpha1.MailboxRemovalList{}
	if err := r.List(ctx, existing); err != nil {
		return fmt.Errorf("list the records of archived mailboxes: %w", err)
	}
	known := map[string]bool{}
	for i := range existing.Items {
		record := &existing.Items[i]
		if record.Spec.Tenant != tenant.Name {
			continue
		}
		// A record of an archive that was deleted since does not stand for
		// the one this restore has just put back.
		switch {
		case record.Status.Phase == gentianov1alpha1.MailboxRemovalArchived:
			known[record.Status.Domain+"/"+record.Status.Archive] = true
		case record.Spec.Restored != nil && record.Status.Phase == "":
			known[record.Spec.Restored.Domain+"/"+record.Spec.Restored.Archive] = true
		}
	}
	for i := range restore.Status.ArchivedMailboxes {
		archive := restore.Status.ArchivedMailboxes[i]
		if known[archive.Domain+"/"+archive.Archive] {
			continue
		}
		sum := sha256.Sum256([]byte(archive.Domain + "/" + archive.Archive + "/" + restore.Namespace + "/" + restore.Name))
		record := &gentianov1alpha1.MailboxRemoval{
			ObjectMeta: metav1.ObjectMeta{
				Name:   tenant.Name + "-" + hex.EncodeToString(sum[:])[:10],
				Labels: map[string]string{tenantLabel: tenant.Name},
			},
			Spec: gentianov1alpha1.MailboxRemovalSpec{
				Tenant: tenant.Name, Address: archive.Address, Mailbox: gentianov1alpha1.MailboxChoiceArchive,
				RequestedBy: gentianov1alpha1.MailboxRequester{Subject: "restore:" + restore.Name, Name: archive.By},
				Restored:    &archive,
			},
		}
		if err := r.Create(ctx, record); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("put the archived mailbox %s on record: %w", archive.Archive, err)
		}
	}
	return nil
}

// ensureRestoreJob creates a unit's Job if absent and reports whether it has
// finished, by the rules a capture goes by (ensureCaptureJob): what is done
// is on record and is not run twice; a Job of the same name that is another
// run's is never taken for this one's; a pod that cannot start is counted,
// because it never fails by itself and the app is paused meanwhile; and the
// attempts are bounded.
//
// A restore used to have none of the last three. A unit whose pod could not
// be created -- a Secret that is not there -- waited for ever with the app
// stopped.
func (r *TenantRestoreReconciler) ensureRestoreJob(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	unit captureUnit,
) (bool, error) {
	entry := appStatus(&restore.Status.Apps, unit.Job.Labels[meta.AppLabel])
	if slices.Contains(entry.CompletedUnits, unit.JobName) {
		return true, nil
	}

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

	if tenantName := tenantNameFromNamespace(restore.Namespace); !runObjectIsOurs(existing.Labels, tenantName, restore.Name) {
		entry.LastFailure = fmt.Sprintf("a Job named %s in %s belongs to another run (tenant %q, run %q)",
			existing.Name, existing.Namespace, existing.Labels[tenantLabel], existing.Labels[backup.ExportLabel])
		entry.Attempts++
		return false, nil
	}

	if jobIsComplete(existing) {
		entry.CompletedUnits = append(entry.CompletedUnits, unit.JobName)
		return true, nil
	}
	// The first reason is kept, and read while there is a pod to read it from.
	if entry.LastFailure == "" && r.Reconciler != nil {
		if reason := r.Reconciler.captureFailureReason(ctx, unit.Job.Namespace, unit.JobName); reason != "" {
			entry.LastFailure = reason
		}
	}
	if r.Reconciler != nil {
		if stall := r.Reconciler.stuckCapture(ctx, existing); stall != "" {
			entry.LastFailure = stall
			entry.Attempts++
			if err := r.Delete(ctx, existing,
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			return false, nil
		}
	}
	if jobIsFailed(existing) {
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

	profiles, err := loadComponentProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	// What the tenant holds for apps it does not have installed: read once,
	// and only when the bundle holds such an app.
	var held *backup.Held
	live := func(app string) (liveApp, error) {
		now := liveApp{}
		for _, a := range tenant.Spec.Apps {
			if a.Profile == app {
				now.installed = true
			}
		}
		profile, ok := appProfileFromIndex(profiles, app)
		if ok {
			now.profile = profile
		}
		if !now.installed {
			// Not installed: what an uninstall left of it, by the rule an
			// export captures it and a purge destroys it by.
			if held == nil {
				read, err := r.Reconciler.heldData(ctx, tenant)
				if err != nil {
					return now, err
				}
				held = &read
			}
			now.held = backup.HeldStores(app, *held)
			now.claims, _ = backup.AppVolumes(held.Claims, tenant, app, now.profile)
			sort.Strings(now.claims)
			return now, nil
		}
		if !ok {
			return now, nil
		}
		claims, err := r.Reconciler.appVolumes(ctx, tenant, app, profile, profileBackupSpec(profile))
		if err != nil {
			return now, fmt.Errorf("list the volume claims of %s: %w", app, err)
		}
		now.claims = claims
		return now, nil
	}
	target, err := r.planTarget(ctx, tenant, restore)
	if err != nil {
		return nil, err
	}
	return planRestore(manifest, tenant, restore.Spec.Apps, restore.Spec.SkipVersionCheck, target, live)
}

// planTarget is where what is the tenant's own goes in the tenant restored
// into: the desktop's database, the mailboxes, the rights store.
func (r *TenantRestoreReconciler) planTarget(ctx context.Context, tenant *gentianov1alpha1.Tenant, restore *gentianov1alpha1.TenantRestore) (planTarget, error) {
	target := planTarget{intoNewTenant: restore.Spec.IntoNewTenant}
	target.desktopDatabase, _ = r.Reconciler.desktopDatabase(tenant)
	if r.Tenant != nil {
		boxes, why, err := r.Tenant.mailboxesOf(ctx, tenant)
		if err != nil {
			return target, refuseRestore("MailboxesUnavailable", "%v. Nothing was changed", err)
		}
		if boxes != nil {
			target.mailDomain = boxes.Domain
		}
		target.noMailboxes = why
		target.cluster = r.Tenant.ClusterID
		target.haveRights = r.Tenant.Rights != nil && r.Tenant.ClusterID != ""
	}
	return target, nil
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
	restore.Status.SourceTenant = plan.sourceTenant
	restore.Status.NameDerivation = plan.derivation
	restore.Status.NotRestored = plan.notRestored
	restore.Status.Notes = restoreLimits(plan.derivation)
	// What the export that wrote the bundle found and did not capture.
	for _, missing := range plan.notIncluded {
		restore.Status.Notes = append(restore.Status.Notes, "The bundle says it does not hold "+missing+".")
	}
	restore.Status.Apps = nil
	for _, app := range plan.apps {
		entry := appStatus(&restore.Status.Apps, app.name)
		entry.Artefacts = app.artefacts
		entry.Stores = artefactKinds(app.artefacts)
		entry.Retained = app.retained
		// Among the result's notes, not on the app's entry: the entry's
		// message is progress and is cleared when the app is done.
		if app.note != "" {
			restore.Status.Notes = append(restore.Status.Notes, app.name+": "+app.note)
		}
	}
	wide := appStatus(&restore.Status.Apps, backupTenantComponent)
	wide.Artefacts = plan.tenantWide
	wide.Stores = artefactKinds(plan.tenantWide)
	restore.Status.Notes = append(restore.Status.Notes, plan.notes...)
	// The rights: what is granted, what is withdrawn, and each entry the
	// bundle holds that is not brought, with the reason.
	restore.Status.ArchivedMailboxes = plan.archivedMailboxes
	restore.Status.Rights = plan.rights
	if plan.rights != nil {
		for _, left := range plan.rights.NotBrought {
			restore.Status.Notes = append(restore.Status.Notes, "A right the bundle holds was NOT brought: "+left+".")
		}
	}
	if plan.sourceTenant != "" && plan.sourceTenant != tenantNameFromNamespace(restore.Namespace) {
		restore.Status.Notes = append(restore.Status.Notes, "The bundle is of tenant "+plan.sourceTenant+
			", not of this one. Every store is restored under this tenant's own names: a database an app made for itself is put back under this tenant's database name, "+
			"the platform's groups under this tenant's, with their members. The bundle's sign-in clients are not imported; this tenant's own were made when its apps were installed.")
	}
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
	// By the mode on record. It used to be read back out of the entry's
	// message, which by now says what the restore is waiting on: an app paused
	// by its maintenance command was scaled, and stayed in maintenance.
	if err := resumeQuiescedApp(ctx, r.Client, r.Tenant,
		tenant.Name, appName, entry.QuiesceMode, entry.Message); err != nil {
		return ctrl.Result{}, fmt.Errorf("resume %s after failure: %w", appName, err)
	}
	unmarkQuiesced(&restore.Status.Quiesced, appName)
	if entry.QuiesceStart != nil && entry.QuiesceEnd == nil {
		entry.QuiesceEnd = ptrNow()
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
	entry.Message = message
	return r.fail(ctx, restore, "RestoreFailed",
		fmt.Sprintf("%s: %s. %s", appName, message, restoreStateText(restore, appName)))
}

// restoreStateText says what state a restore that stopped has left each
// app's data in: which are restored, which one it was at, and which it never
// reached. The one it was at is the one to look at: a database is loaded in
// one transaction and is either the bundle's or what it was, a bucket or a
// volume is written object by object and file by file and can be part of each.
func restoreStateText(restore *gentianov1alpha1.TenantRestore, at string) string {
	var done, untouched []string
	for _, entry := range restore.Status.Apps {
		if entry.Name == at || entry.Name == backupTenantComponent {
			continue
		}
		if entry.Phase == gentianov1alpha1.TenantExportPhaseReady {
			done = append(done, entry.Name)
		} else {
			untouched = append(untouched, entry.Name)
		}
	}
	text := "Every app this restore paused is running again. Restored: " + listOrNothing(done) + ". "
	if at == backupTenantComponent {
		text += "The realm and the desktop's database may be part restored"
	} else {
		text += "The data of " + at + " may be part restored (a database is replaced whole or not at all; a bucket or a volume can be part written)"
		wide := appStatus(&restore.Status.Apps, backupTenantComponent)
		if wide.Phase != gentianov1alpha1.TenantExportPhaseReady {
			untouched = append(untouched, "the realm and the desktop's database")
		}
	}
	return text + ". Not touched: " + listOrNothing(untouched) + ". Start a new restore to finish"
}

func listOrNothing(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	return strings.Join(items, ", ")
}

// restoreFinalizer holds a TenantRestore until what it paused is running
// again and what it staged is gone.
const restoreFinalizer = "gentianos.io/tenantrestore-resume"

// finalize is the deletion path. Deleting a restore is the only way to stop
// one that is running; without this the app it was at stayed paused, and its
// Jobs went on replacing data nobody was waiting for.
func (r *TenantRestoreReconciler) finalize(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenantName string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithName("tenantrestore")

	// Stop the Jobs first: a unit still loading would go on writing into an
	// app that is about to be started.
	if err := r.deleteRestoreJobs(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	_ = r.discardStagedRestoreSecrets(ctx, restore)

	if !restore.IsTerminal() && restore.Status.StartedAt != nil {
		logger.Info("restore deleted while it ran: its Jobs were stopped and the apps it paused are resumed",
			"restore", restore.Name, "tenant", tenantName, "state", restoreStateText(restore, currentRestoreApp(restore)))
	}
	// Resume, always. Only a restore whose tenant is itself going is let go
	// with an app it could not resume: the workloads are going too, and
	// holding the restore would hold the namespace. Whether the tenant is
	// going has to be known for that -- not being able to ask is not a yes.
	if err := r.resumeAll(ctx, restore, tenantName); err != nil {
		going, askErr := r.tenantGoing(ctx, restore.Namespace, tenantName)
		if askErr != nil || !going {
			return ctrl.Result{}, err
		}
		logger.Info("an app this restore paused could not be resumed; the tenant is going, so the restore is not held for it",
			"restore", restore.Name, "tenant", tenantName, "error", err.Error())
	}
	controllerutil.RemoveFinalizer(restore, restoreFinalizer)
	return ctrl.Result{}, r.Update(ctx, restore)
}

// tenantGoing reports whether the tenant, or its namespace, is gone or being
// deleted. An answer that could not be had is an error.
func (r *TenantRestoreReconciler) tenantGoing(ctx context.Context, namespace, tenantName string) (bool, error) {
	ns := &corev1.Namespace{}
	switch err := r.Get(ctx, types.NamespacedName{Name: namespace}, ns); {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	case ns.DeletionTimestamp != nil:
		return true, nil
	}
	tenant := &gentianov1alpha1.Tenant{}
	switch err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	}
	return tenant.DeletionTimestamp != nil, nil
}

// currentRestoreApp is the app a running restore is at: the first planned one
// that is not done, or the tenant's own once every app is.
func currentRestoreApp(restore *gentianov1alpha1.TenantRestore) string {
	if current := nextPendingApp(restore.Status.Apps, plannedApps(restore)); current != "" {
		return current
	}
	return backupTenantComponent
}

// deleteRestoreJobs removes every Job of this restore, in every namespace a
// unit runs in, by the tenant's label and the restore's.
func (r *TenantRestoreReconciler) deleteRestoreJobs(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	tenantName := tenantNameFromNamespace(restore.Namespace)
	for _, ns := range runNamespaces(restore.Namespace) {
		jobs := &batchv1.JobList{}
		if err := r.List(ctx, jobs,
			client.InNamespace(ns),
			client.MatchingLabels{backup.ExportLabel: restore.Name, tenantLabel: tenantName}); err != nil {
			return fmt.Errorf("list the Jobs of restore %s in %s: %w", restore.Name, ns, err)
		}
		for i := range jobs.Items {
			if err := r.Delete(ctx, &jobs.Items[i],
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete restore Job %s: %w", jobs.Items[i].Name, err)
			}
		}
	}
	return nil
}

// stagedDecryptionSecretName names the kernel-namespace copy of the
// operator-supplied key material (see stageDecryptionSecret).
func stagedDecryptionSecretName(tenantName, restoreName string) string {
	return "trs-" + tenantName + "-" + restoreName + "-key"
}

// discardStagedRestoreSecrets removes every staged copy: the decryption key
// beside the object store, and the credentials and key staged wherever a unit
// ran. The copy beside the object store leaking past the restore was a real
// gap — the identity is escrowed off-cluster precisely so the cluster does
// not hold it.
func (r *TenantRestoreReconciler) discardStagedRestoreSecrets(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	keyErr := discardStagedSecret(ctx, r.Client,
		stagedDecryptionSecretName(tenantNameFromNamespace(restore.Namespace), restore.Name), s3Namespace)
	stagedErr := r.discardRestoreStagedSecrets(ctx, restore)
	if keyErr != nil {
		return keyErr
	}
	return stagedErr
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

	tenantName := tenantNameFromNamespace(restore.Namespace)
	name := stagedDecryptionSecretName(tenantName, restore.Name)
	return name, putRunSecret(ctx, r.Client, runSecret(name, s3Namespace, tenantName, restore.Name,
		map[string][]byte{key: value}))
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
