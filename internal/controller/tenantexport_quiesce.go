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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// replicaMemoAnnotation remembers what a workload was scaled to before an
// export paused it.
//
// It lives on the workload rather than in the TenantExport's status because the
// workload is the thing that has to be put back: if the export is deleted
// mid-flight, or its status write is the one that is lost, the annotation is
// still there for the next reconcile to read. Storing the original count only
// in the export would mean a deleted export leaves an app scaled to zero with
// nothing anywhere recording what it should be.
const replicaMemoAnnotation = "gentianos.io/pre-export-replicas"

// quiesceApp pauses an app's writes and reports the mode actually used.
//
// command mode runs the profile's own maintenance hooks, which pauses writes
// without taking the app offline — users see a maintenance page instead of a
// connection error. It needs a ready pod and a configured execer; when either
// is missing this falls back to scaling down and says so, because the data
// guarantee is identical either way and refusing to back an app up because its
// pod is unhealthy would be the wrong trade.
//
// The mode returned is the one used, not the one requested, so the manifest
// records what actually happened.
func (r *TenantReconciler) quiesceApp(ctx context.Context, tenantName, appName string, spec *gentianov1alpha1.BackupSpec) (gentianov1alpha1.BackupQuiesceMode, error) {
	requested := spec.QuiesceMode()
	if requested == gentianov1alpha1.BackupQuiesceNone {
		return requested, nil
	}

	if requested == gentianov1alpha1.BackupQuiesceCommand {
		pre, _ := spec.QuiesceCommands()
		if len(pre) > 0 && r.Exec != nil {
			if _, err := r.execAppCommand(ctx, tenantName, appName, spec, pre); err == nil {
				return gentianov1alpha1.BackupQuiesceCommand, nil
			}
			// Fall through: an app whose maintenance hook cannot run still has
			// to be captured consistently, and scaling down achieves that.
		}
	}

	if err := r.scaleAppWorkloads(ctx, tenantName, appName, 0); err != nil {
		return requested, err
	}
	return gentianov1alpha1.BackupQuiesceScaleDown, nil
}

// unquiesceApp reverses quiesceApp for the mode that was actually used.
func (r *TenantReconciler) unquiesceApp(
	ctx context.Context,
	tenantName, appName string,
	spec *gentianov1alpha1.BackupSpec,
	used gentianov1alpha1.BackupQuiesceMode,
) error {
	if used == gentianov1alpha1.BackupQuiesceCommand {
		_, post := spec.QuiesceCommands()
		if len(post) > 0 && r.Exec != nil {
			if _, err := r.execAppCommand(ctx, tenantName, appName, spec, post); err != nil {
				// Leaving an app in maintenance mode is an outage, so a failed
				// resume hook is not the end of the attempt: scaling the app is
				// still tried below, which restarts it out of maintenance.
				return r.scaleAppWorkloads(ctx, tenantName, appName, -1)
			}
			return nil
		}
	}
	return r.scaleAppWorkloads(ctx, tenantName, appName, -1)
}

// resumeQuiescedApp reverses a pause the way it was actually made.
//
// The recorded mode decides: an app paused by its maintenance hook must have
// its resume hook run — merely scaling it leaves the maintenance flag set and
// the app serving its maintenance page forever, with the export status
// claiming it resumed. That is not hypothetical: the first live command-mode
// capture did exactly this to Nextcloud. The profile is re-resolved here
// because resume can happen many reconciles after the pause, including after
// an operator restart; when the tenant or profile is gone the scale fallback
// still runs, which is the right floor.
func resumeQuiescedApp(
	ctx context.Context,
	c client.Client,
	tr *TenantReconciler,
	tenantName, appName, recordedMode, message string,
) error {
	mode := gentianov1alpha1.BackupQuiesceMode(recordedMode)
	if mode == "" {
		mode = quiesceModeFromMessage(message)
	}
	var spec *gentianov1alpha1.BackupSpec
	tenant := &gentianov1alpha1.Tenant{}
	if err := c.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); err == nil {
		if profile, perr := resolveProfile(ctx, c, tenant, appName); perr == nil {
			spec = profileBackupSpec(profile)
		}
	}
	return tr.unquiesceApp(ctx, tenantName, appName, spec, mode)
}

// resumeApp puts an app back. It is safe to call when the app was never paused,
// and safe to call twice: both are normal, because it runs on the failure path
// and on every reconcile that finds a stale entry in status.quiesced.
func (r *TenantReconciler) resumeApp(ctx context.Context, tenantName, appName string) error {
	return r.scaleAppWorkloads(ctx, tenantName, appName, -1)
}

// scaleAppWorkloads scales an app's Deployments and StatefulSets. A replicas of
// -1 means "restore whatever was memoed", which is how resume works.
func (r *TenantReconciler) scaleAppWorkloads(ctx context.Context, tenantName, appName string, replicas int32) error {
	ns := backup.TenantNamespace(tenantName)

	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if !workloadBelongsToApp(d.Labels, d.Name, appName) {
			continue
		}
		if err := r.scaleOne(ctx, d, d.Spec.Replicas, replicas, func(v *int32) { d.Spec.Replicas = v }); err != nil {
			return err
		}
	}

	statefulSets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list statefulsets in %s: %w", ns, err)
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		if !workloadBelongsToApp(s.Labels, s.Name, appName) {
			continue
		}
		if err := r.scaleOne(ctx, s, s.Spec.Replicas, replicas, func(v *int32) { s.Spec.Replicas = v }); err != nil {
			return err
		}
	}
	return nil
}

func (r *TenantReconciler) scaleOne(
	ctx context.Context,
	obj client.Object,
	current *int32,
	target int32,
	set func(*int32),
) error {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	if target >= 0 {
		// Pausing. Only memo the original count the first time: a second pass
		// over an already-paused workload would otherwise record 0 as its
		// desired size and resume it to nothing.
		if _, memoed := annotations[replicaMemoAnnotation]; !memoed {
			original := int32(1)
			if current != nil {
				original = *current
			}
			annotations[replicaMemoAnnotation] = strconv.Itoa(int(original))
			obj.SetAnnotations(annotations)
		}
		if current != nil && *current == target {
			// Already at the target, but the memo may have just been added.
			return r.Update(ctx, obj)
		}
		scaled := target
		set(&scaled)
		return r.Update(ctx, obj)
	}

	// Resuming.
	memo, ok := annotations[replicaMemoAnnotation]
	if !ok {
		// Never paused by us. Leave it alone — scaling a workload we did not
		// scale down would be a change nobody asked for.
		return nil
	}
	original, err := strconv.Atoi(memo)
	if err != nil || original < 0 {
		original = 1
	}
	delete(annotations, replicaMemoAnnotation)
	obj.SetAnnotations(annotations)
	restored := int32(original)
	set(&restored)
	return r.Update(ctx, obj)
}

// workloadBelongsToApp matches a workload to an installed app.
//
// It reuses the same label conventions the volume matcher does, minus the
// bare-name substring fallback: scaling the wrong workload takes an unrelated
// app offline, so this errs toward missing a workload rather than pausing a
// neighbour's.
func workloadBelongsToApp(labels map[string]string, objectName, appName string) bool {
	if labels["gentianos.io/app"] == appName {
		return true
	}
	if instance, ok := labels["app.kubernetes.io/instance"]; ok {
		if instance == appName || len(instance) > len(appName) && instance[:len(appName)] == appName {
			return true
		}
	}
	if name, ok := labels["app.kubernetes.io/name"]; ok && name == appName {
		return true
	}
	return objectName == appName
}
