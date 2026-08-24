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
	"k8s.io/apimachinery/pkg/api/equality"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// tenantBlockedRequeueAfter is how long to wait before retrying a Tenant whose
// stage reported blocked without naming its own interval.
//
// 30s matches the platform gateway reconciler's retry on an unsatisfied
// dependency. Blocked preconditions are usually resolved by something outside the
// Tenant's watches — a cluster admin approving a waiver, an AppProfile being
// published, ESO syncing a Secret — so this is a poll, and the interval trades
// how long a fixed cluster stays Degraded against reconcile churn per tenant.
const tenantBlockedRequeueAfter = 30 * time.Second

const (
	// How long a tenant may go without any condition transition before its
	// requeue interval is stretched. A stage asks to be run again in about two
	// seconds because that is the right pace for something about to finish; it
	// is the wrong pace for something that has been waiting on the same
	// external fact for ten minutes, and the reconciler cannot tell the two
	// apart from inside a single stage.
	//
	// A tenant that keeps a worker busy every two seconds is not free. Work
	// arriving once — a deletion above all — waits behind every tenant still
	// converging, and the more tenants a cluster has, the longer it waits.
	tenantConvergenceFastWindow = 1 * time.Minute
	tenantConvergenceSlowWindow = 5 * time.Minute

	// Stretched, not abandoned. Something that converges after twenty minutes
	// is still noticed within half a minute of doing so, and anything that
	// reports its own progress wakes the tenant immediately through a watch
	// rather than through this timer.
	tenantConvergenceMaxRequeue = 30 * time.Second
)

// tenantConvergenceRequeue scales a stage's requested requeue by how long the
// tenant has gone without a condition transition. A transition is this
// controller's only general-purpose evidence that something moved, so a tenant
// that is making progress keeps the pace the stage asked for and one that is
// stuck backs off.
func tenantConvergenceRequeue(tenant *gentianov1alpha1.Tenant, base time.Duration, now time.Time) time.Duration {
	if base <= 0 {
		return base
	}
	var latest time.Time
	for _, condition := range tenant.Status.Conditions {
		if condition.LastTransitionTime.After(latest) {
			latest = condition.LastTransitionTime.Time
		}
	}
	// No transition recorded yet means the tenant has only just arrived, which
	// is when the stage's own pace is most likely to be right.
	if latest.IsZero() {
		return base
	}

	var scaled time.Duration
	switch stalled := now.Sub(latest); {
	case stalled < tenantConvergenceFastWindow:
		return base
	case stalled < tenantConvergenceSlowWindow:
		scaled = base * 4
	default:
		scaled = base * 15
	}
	if scaled > tenantConvergenceMaxRequeue {
		return tenantConvergenceMaxRequeue
	}
	return scaled
}

// ReconcileStage names a tenant reconcile pipeline phase. Stages run in order;
// the first stage that returns RequeueAfter or error short-circuits the pipeline.
type ReconcileStage string

const (
	StagePreflight    ReconcileStage = "Preflight"
	StageBootstrap    ReconcileStage = "Bootstrap"
	StageDataPlane    ReconcileStage = "DataPlane"
	StageAppsAndEdge  ReconcileStage = "AppsAndEdge"
	StageIntegrations ReconcileStage = "Integrations"
	StageSharedKernel ReconcileStage = "SharedKernel"
	StageFinalize     ReconcileStage = "Finalize"
)

type tenantReconcileState struct {
	tenant          *gentianov1alpha1.Tenant
	nsName          string
	start           time.Time
	blocked         bool
	identityResult  ctrl.Result
	databaseResult  ctrl.Result
	mariadbResult   ctrl.Result
	storageResult   ctrl.Result
	cacheResult     ctrl.Result
	mailResult      ctrl.Result
	appsResult      ctrl.Result
	privilegeResult ctrl.Result

	// statusAtEntry is the Tenant status as this reconcile found it, so a
	// short-circuit can tell a write that changes something from one that
	// changes nothing. Set by runTenantReconcileStages before any stage runs:
	// taken later it would already contain that stage's own edits, and those
	// edits would then never be written.
	statusAtEntry *gentianov1alpha1.TenantStatus
}

func (r *TenantReconciler) runTenantReconcileStages(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	state.statusAtEntry = state.tenant.Status.DeepCopy()
	stages := []struct {
		stage ReconcileStage
		run   func(context.Context, *tenantReconcileState) (ctrl.Result, error)
	}{
		{StagePreflight, r.reconcileTenantStagePreflight},
		{StageBootstrap, r.reconcileTenantStageBootstrap},
		{StageDataPlane, r.reconcileTenantStageDataPlane},
		{StageAppsAndEdge, r.reconcileTenantStageAppsAndEdge},
		{StageIntegrations, r.reconcileTenantStageIntegrations},
		{StageSharedKernel, r.reconcileTenantStageSharedKernel},
	}
	for _, step := range stages {
		res, err := step.run(ctx, state)
		if state.blocked {
			// Requeue rather than stopping dead.
			//
			// Blocked means a stage found a precondition it cannot satisfy itself —
			// a missing AppProfile, an unapproved waiver, a Secret that has not
			// synced. Returning an empty Result meant nothing was scheduled, so the
			// Tenant only moved again if some watched object happened to fire an
			// event. For a precondition owned by something the Tenant does not
			// watch, that is never, and the Tenant sat Degraded indefinitely with
			// the cluster otherwise healthy.
			//
			// Preserve an explicit RequeueAfter from the stage: a stage that knows
			// how long to wait knows better than this default.
			if res.RequeueAfter > 0 {
				return res, nil
			}
			return ctrl.Result{RequeueAfter: tenantBlockedRequeueAfter}, nil
		}
		if res.RequeueAfter > 0 || err != nil {
			if res.RequeueAfter > 0 && err == nil {
				// Re-read the composite before persisting. Only two places
				// evaluated CrossplaneReady: the waitForTenantShell early return
				// and the finalize stage. Once the shell was ready the first
				// stopped running, and any stage in between that requeued
				// returned here without ever reaching the second — so the
				// condition stayed frozen at what it was moments after the
				// XTenant was created, when it genuinely had no status yet.
				// The composite could converge fully with the Tenant still
				// reporting that it had no status at all, and nothing would
				// revisit that until the caller timed out and rolled back a
				// tenant that had actually finished provisioning.
				//
				// persistTenantStageProgress reads this condition to pick the
				// phase, so refreshing it first fixes the phase too.
				// Only while the answer can still change. Re-reading the
				// composite costs an API round trip inside the reconcile —
				// unstructured reads do not come from the cache — and a
				// converged tenant that requeues every two seconds would pay it
				// forever for an answer that is already True. With a bounded
				// worker pool that backlog is what starves the tenants waiting
				// on a first reconcile, a deletion among them.
				//
				// True going False again is still caught: finalize re-evaluates
				// on any reconcile that reaches it, and the XTenant watch wakes
				// one. The direction that hangs an install is False going True,
				// and that is the direction this keeps checking.
				if !tenantHasConditionTrue(state.tenant, conditionCrossplaneReady) {
					_ = r.aggregateCrossplaneStatus(ctx, state.tenant)
				}
				if updErr := r.persistTenantStageProgress(ctx, state); updErr != nil {
					return ctrl.Result{}, updErr
				}
			}
			if res.RequeueAfter > 0 {
				res.RequeueAfter = tenantConvergenceRequeue(state.tenant, res.RequeueAfter, time.Now())
				log.FromContext(ctx).Info("tenant reconcile short-circuit", "stage", step.stage, "tenant", state.tenant.Name, "requeueAfter", res.RequeueAfter)
			}
			return res, err
		}
	}
	// Finalize has a return per data-plane stage; the pacing applies to all of
	// them, so it is applied to what finalize returns rather than at each one.
	finalRes, finalErr := r.reconcileTenantStageFinalize(ctx, state)
	if finalRes.RequeueAfter > 0 {
		finalRes.RequeueAfter = tenantConvergenceRequeue(state.tenant, finalRes.RequeueAfter, time.Now())
	}
	return finalRes, finalErr
}

// persistTenantStageProgress writes in-memory condition and phase updates when a
// stage short-circuits with RequeueAfter before finalize runs.
func (r *TenantReconciler) persistTenantStageProgress(ctx context.Context, state *tenantReconcileState) error {
	tenant := state.tenant
	if state.nsName != "" {
		tenant.Status.Namespace = state.nsName
		tenant.Status.AdminEmail = r.tenantAdminEmail(tenant)
	}
	tenant.Status.AppCount = len(tenant.Spec.Apps)
	provisioning := state.identityResult.RequeueAfter > 0 ||
		state.databaseResult.RequeueAfter > 0 ||
		state.mariadbResult.RequeueAfter > 0 ||
		state.storageResult.RequeueAfter > 0 ||
		state.cacheResult.RequeueAfter > 0 ||
		state.mailResult.RequeueAfter > 0 ||
		state.appsResult.RequeueAfter > 0
	if provisioning || !tenantHasConditionTrue(tenant, conditionCrossplaneReady) {
		if tenant.Status.Phase != gentianov1alpha1.TenantPhaseDegraded {
			tenant.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
		}
	}

	// A write that changes nothing is not free: this controller watches Tenant,
	// so every status update it makes is an event that wakes it again. A tenant
	// requeueing every two seconds while it converges therefore re-reconciles
	// itself on top of its own timer, and with a bounded worker pool the tenants
	// doing that starve the ones waiting for a first reconcile — a deletion
	// among them. Writing only real changes keeps the timer as the only clock.
	if state.statusAtEntry != nil && equality.Semantic.DeepEqual(state.statusAtEntry, &tenant.Status) {
		return nil
	}
	return r.Status().Update(ctx, tenant)
}

func (r *TenantReconciler) reconcileTenantStagePreflight(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	missingProfiles, err := r.validateTenantPrerequisites(ctx, tenant)
	if err != nil {
		reconcileErrors.WithLabelValues("tenant").Inc()
		return ctrl.Result{}, err
	}
	if len(missingProfiles) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "ProfileNotFound",
			fmt.Sprintf("AppProfile(s) not found: %s", strings.Join(missingProfiles, ", ")))
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse, "PrerequisitesFailed",
			"Identity provisioning blocked because one or more requested AppProfiles are missing")
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		state.blocked = true
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, nil
	}

	if err := tenancy.EnforceSingle(ctx, r.Client, r.TenancyMode, tenant); err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "TenancyConstraint", err.Error())
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		state.blocked = true
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageBootstrap(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	state.nsName = tenantNamespaceName(tenant)
	log.FromContext(ctx).Info("reconciling tenant", "tenant", tenant.Name, "namespace", state.nsName, "crossplaneOnly", r.CrossplaneOnly)

	if err := r.ensureTenantProvisioningManifests(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureTenantXR(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if res, err := r.waitForTenantShell(ctx, tenant, state.nsName); res.RequeueAfter > 0 || err != nil {
		_ = r.aggregateCrossplaneStatus(ctx, tenant)
		r.updateBlockedStatus(ctx, tenant)
		return res, err
	}
	if err := r.ensureNetworkPolicies(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRegistryCredentials(ctx, tenant, state.nsName); err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "EntitlementRequired", err.Error())
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		state.blocked = true
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, fmt.Errorf("registry credentials error: %w", err)
	}
	if err := r.ensureStagingCaTrust(ctx, tenant, state.nsName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageDataPlane(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant

	identityResult, err := r.ensureIdentity(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.identityResult = identityResult
	if identityResult.RequeueAfter > 0 {
		return identityResult, nil
	}

	databaseResult, err := r.ensureDatabase(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.databaseResult = databaseResult
	if databaseResult.RequeueAfter > 0 {
		return databaseResult, nil
	}

	mariadbResult, err := r.ensureMariaDB(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.mariadbResult = mariadbResult
	if mariadbResult.RequeueAfter > 0 {
		return mariadbResult, nil
	}

	storageResult, err := r.ensureStorage(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.storageResult = storageResult
	if storageResult.RequeueAfter > 0 {
		return storageResult, nil
	}

	cacheResult, err := r.ensureCache(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.cacheResult = cacheResult
	return cacheResult, nil
}

func (r *TenantReconciler) reconcileTenantStageAppsAndEdge(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	if _, err := r.ensureMacWaivers(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure mac waivers: %w", err)
	}

	// Tenant drop-ins (customization ladder L1 at tenant scope) must exist before
	// the app workload is created, so the mount is populated on first start.
	if _, err := r.ensureTenantDropIns(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure tenant drop-ins: %w", err)
	}

	appsResult, err := r.reconcileTenantApps(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.appsResult = appsResult

	privilegeResult, err := r.ensureAppPrivileges(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.privilegeResult = privilegeResult

	if _, err := r.ensureGateway(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageIntegrations(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	if _, err := r.ensureIntegrationBindings(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	if _, err := r.ensureAppGrants(ctx, tenant); err != nil {
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, fmt.Errorf("ensure app grants: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageSharedKernel(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	if r.CrossplaneOnly {
		return ctrl.Result{}, nil
	}
	tenant := state.tenant
	logger := log.FromContext(ctx)
	mailResult, err := r.ensureMail(ctx, tenant)
	if err != nil {
		r.updateBlockedStatus(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.mailResult = mailResult
	if mailResult.RequeueAfter > 0 {
		return mailResult, nil
	}

	if err := r.ensurePortalRedirect(ctx, tenant); err != nil {
		logger.Error(err, "ensure shared portal convergence (non-blocking, will retry)")
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	if err := r.ensureKeycloakBrowserSecurityHeaders(ctx, tenant); err != nil {
		logger.Error(err, "ensure Keycloak browser security headers (non-blocking, will retry)")
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	// LiteLLM team for this tenant, if the cluster serves LLMs at all.
	// Deliberately not a blocker: see ensureTenantLiteLLMTeam.
	r.ensureTenantLiteLLMTeam(ctx, tenant)
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageFinalize(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	logger := log.FromContext(ctx)

	if err := r.aggregateCrossplaneStatus(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionTrue, "Provisioned", "Tenant namespace is ready")
	tenant.Status.Namespace = state.nsName
	tenant.Status.AdminEmail = r.tenantAdminEmail(tenant)
	tenant.Status.AppCount = len(tenant.Spec.Apps)
	tenant.Status.ReadyApps = len(tenant.Status.ProvisionedApps)

	provisioning := state.identityResult.RequeueAfter > 0 ||
		state.databaseResult.RequeueAfter > 0 ||
		state.mariadbResult.RequeueAfter > 0 ||
		state.storageResult.RequeueAfter > 0 ||
		state.cacheResult.RequeueAfter > 0
	crossplaneReady := tenantHasConditionTrue(tenant, conditionCrossplaneReady)
	// The identity and data-plane conditions must be present and True, whatever
	// the stages asked for. See tenantFoundationNotReady: requeues answer "did a
	// stage want to run again", which is not the same question as "is this tenant
	// ready", and the two disagreed for any tenant that finalized before its
	// AppProfiles resolved.
	notReady := tenantFoundationNotReady(tenant)

	switch {
	case provisioning || !crossplaneReady || notReady != "":
		if notReady != "" && !provisioning && crossplaneReady {
			// Worth saying which one: this is the case that used to read Ready,
			// so a phase that stays Provisioning with everything else quiet
			// would otherwise be the only symptom.
			logger.Info("tenant not ready", "tenant", tenant.Name, "condition", notReady)
		}
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
	default:
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseReady
		provisioningDuration.WithLabelValues(tenant.Name).Observe(time.Since(state.start).Seconds())
	}
	tenantAppsTotal.WithLabelValues(tenant.Name).Set(float64(tenant.Status.AppCount))
	if err := r.Status().Update(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	if provisioning {
		logger.Info("tenant provisioning in progress", "tenant", tenant.Name)
		if state.identityResult.RequeueAfter > 0 {
			return state.identityResult, nil
		}
		if state.databaseResult.RequeueAfter > 0 {
			return state.databaseResult, nil
		}
		if state.mariadbResult.RequeueAfter > 0 {
			return state.mariadbResult, nil
		}
		if state.storageResult.RequeueAfter > 0 {
			return state.storageResult, nil
		}
		if state.cacheResult.RequeueAfter > 0 {
			return state.cacheResult, nil
		}
		if state.mailResult.RequeueAfter > 0 {
			return state.mailResult, nil
		}
		return state.appsResult, nil
	}
	if !crossplaneReady {
		logger.Info("tenant operator paths ready; waiting for Crossplane XTenant Ready", "tenant", tenant.Name)
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	if state.mailResult.RequeueAfter > 0 {
		logger.Info("tenant ready; mail still converging", "tenant", tenant.Name)
		return state.mailResult, nil
	}
	if state.appsResult.RequeueAfter > 0 {
		logger.Info("tenant ready; apps still converging", "tenant", tenant.Name)
		return state.appsResult, nil
	}
	if state.privilegeResult.RequeueAfter > 0 {
		logger.Info("tenant ready; app privilege sync scheduled", "tenant", tenant.Name)
		return state.privilegeResult, nil
	}
	if notReady != "" {
		// A condition is still not True and nothing above asked to be run again.
		// An empty Result here would leave the tenant Provisioning until some
		// watched object happened to fire an event — the trap the blocked path
		// at the top of runTenantReconcileStages exists to avoid, reached by a
		// different route.
		logger.Info("tenant waiting on condition", "tenant", tenant.Name, "condition", notReady)
		return ctrl.Result{RequeueAfter: tenantShellRequeueAfter}, nil
	}
	logger.Info("tenant reconciled successfully", "tenant", tenant.Name)
	return ctrl.Result{}, nil
}

// updateBlockedStatus writes the Degraded phase and the conditions explaining why,
// and logs a failure instead of discarding it.
//
// These sites previously did `_ = r.Status().Update(...)`. The write is genuinely
// non-fatal — the stage has already decided to block, and the next reconcile
// recomputes the same status — but silently dropping it meant a conflict left the
// Tenant reporting the wrong phase with no trace of why, which is the state that
// makes a stuck tenant unexplainable. Not returned as an error, because a stale
// status is not a reason to abandon the reconcile that produced it.
func (r *TenantReconciler) updateBlockedStatus(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	if err := r.Status().Update(ctx, tenant); err != nil {
		log.FromContext(ctx).Error(err, "recording blocked tenant status",
			"tenant", tenant.Name, "phase", tenant.Status.Phase)
	}
}
