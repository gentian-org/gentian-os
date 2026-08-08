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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

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
}

func (r *TenantReconciler) runTenantReconcileStages(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
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
			return ctrl.Result{}, nil
		}
		if res.RequeueAfter > 0 || err != nil {
			if res.RequeueAfter > 0 && err == nil {
				if updErr := r.persistTenantStageProgress(ctx, state); updErr != nil {
					return ctrl.Result{}, updErr
				}
			}
			if res.RequeueAfter > 0 {
				log.FromContext(ctx).Info("tenant reconcile short-circuit", "stage", step.stage, "tenant", state.tenant.Name, "requeueAfter", res.RequeueAfter)
			}
			return res, err
		}
	}
	return r.reconcileTenantStageFinalize(ctx, state)
}

// persistTenantStageProgress writes in-memory condition and phase updates when a
// stage short-circuits with RequeueAfter before finalize runs.
func (r *TenantReconciler) persistTenantStageProgress(ctx context.Context, state *tenantReconcileState) error {
	tenant := state.tenant
	if state.nsName != "" {
		tenant.Status.Namespace = state.nsName
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
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, nil
	}

	if err := r.validateTenancyConstraints(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "TenancyConstraint", err.Error())
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		state.blocked = true
		_ = r.Status().Update(ctx, tenant)
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
		_ = r.Status().Update(ctx, tenant)
		return res, err
	}
	if err := r.ensureNetworkPolicies(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRegistryCredentials(ctx, tenant, state.nsName); err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "EntitlementRequired", err.Error())
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		state.blocked = true
		_ = r.Status().Update(ctx, tenant)
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
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.identityResult = identityResult
	if identityResult.RequeueAfter > 0 {
		return identityResult, nil
	}

	databaseResult, err := r.ensureDatabase(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.databaseResult = databaseResult
	if databaseResult.RequeueAfter > 0 {
		return databaseResult, nil
	}

	mariadbResult, err := r.ensureMariaDB(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.mariadbResult = mariadbResult
	if mariadbResult.RequeueAfter > 0 {
		return mariadbResult, nil
	}

	storageResult, err := r.ensureStorage(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.storageResult = storageResult
	if storageResult.RequeueAfter > 0 {
		return storageResult, nil
	}

	cacheResult, err := r.ensureCache(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
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

	appsResult, err := r.ensureAppDeployment(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.appsResult = appsResult

	privilegeResult, err := r.ensureAppPrivileges(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	state.privilegeResult = privilegeResult

	if _, err := r.ensureGateway(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) reconcileTenantStageIntegrations(ctx context.Context, state *tenantReconcileState) (ctrl.Result, error) {
	tenant := state.tenant
	if _, err := r.ensureIntegrationBindings(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}
	if _, err := r.ensureAppGrants(ctx, tenant); err != nil {
		_ = r.Status().Update(ctx, tenant)
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
		_ = r.Status().Update(ctx, tenant)
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
	tenant.Status.AppCount = len(tenant.Spec.Apps)
	tenant.Status.ReadyApps = len(tenant.Status.ProvisionedApps)

	provisioning := state.identityResult.RequeueAfter > 0 ||
		state.databaseResult.RequeueAfter > 0 ||
		state.mariadbResult.RequeueAfter > 0 ||
		state.storageResult.RequeueAfter > 0 ||
		state.cacheResult.RequeueAfter > 0
	crossplaneReady := tenantHasConditionTrue(tenant, conditionCrossplaneReady)

	if provisioning || !crossplaneReady {
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
	} else {
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
	logger.Info("tenant reconciled successfully", "tenant", tenant.Name)
	return ctrl.Result{}, nil
}
