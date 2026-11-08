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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
)

// The system engines on demand (kernel.md §7, app-authorization.md §3). The system tier
// runs an engine that is not on every cluster only once something declares
// it: the operator keeps, per cluster, the set of engines that are wanted, and
// the Cluster composition composes each one the set names.
//
// The set is the ConfigMap systemEnginesConfigMap in the control namespace,
// one key per engine:
//
//	<engine>: wanted   a component declares it, or did
//	<engine>: purge    a person asked for it to go and nothing declares it
//
// and, written by a person, purge.<engine> to ask for that. An engine is
// never taken out of the set because the last component that declared it
// went: its data may be all that is left of a removed app.
const (
	systemEnginesConfigMap = "system-engines"
	// systemEnginesConfigType is the label the Cluster composition selects
	// the set by (function-extra-resources, fetch-system-engines).
	systemEnginesConfigType = "system-engines"
	engineWanted            = "wanted"
	enginePurge             = "purge"
	// enginePurgeRequestPrefix is the key a person writes to ask for an
	// engine to go: purge.mariadb, with any value.
	enginePurgeRequestPrefix = "purge."

	reasonSystemEngineNotReady = "SystemEngineNotReady"
)

// The engines that are composed on demand. PostgreSQL and the object store
// are on every cluster and are not among them: the kernel's UIs keep their
// data on the one, and every backup, bundle and export is written to the
// other.
const (
	EngineMariaDB       = "mariadb"
	EngineCache         = "cache"
	EngineAuthorization = "authorization"
	EngineTURN          = "turn"
)

// engineNamespace is the namespace an engine runs in. It is named for the
// engine's function, system-<name>, except for an engine that is all edge:
// TURN faces the internet and holds nothing, so its whole namespace is a DMZ.
func engineNamespace(engine string) string {
	if engine == EngineTURN {
		return turnEdgeNamespace
	}
	return layout.System(engine)
}

// systemEngines are the on-demand engines, by name, each with what decides
// whether a profile declares it. The name is the system namespace's
// function as well (engineNamespace).
var systemEngines = []struct {
	name     string
	declares func(*gentianov1alpha1.ComponentProfile) bool
}{
	{EngineMariaDB, func(p *gentianov1alpha1.ComponentProfile) bool {
		return provisioner.DatabaseEngineOf(p) == gentianov1alpha1.DatabaseEngineMariaDB
	}},
	{EngineCache, func(p *gentianov1alpha1.ComponentProfile) bool {
		return provisioner.CacheEngineOf(p) == gentianov1alpha1.CacheEngineRedis
	}},
	{EngineAuthorization, wantsAuthorization},
	{EngineTURN, wantsTURN},
}

// declaredEngines are the on-demand engines a profile declares.
func declaredEngines(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil {
		return nil
	}
	var out []string
	for _, e := range systemEngines {
		if e.declares(profile) {
			out = append(out, e.name)
		}
	}
	return out
}

// SystemEnginesReconciler keeps the set of system engines this cluster runs.
type SystemEnginesReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it reads: a profile read through a
	// definition that lost a requirement declares no engine for it. Nil
	// holds nothing.
	Definitions *crdcheck.Holder
	client.Client
	// Namespace is the control namespace, where the set is written.
	Namespace string
	Recorder  record.EventRecorder
}

var systemEnginesRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: systemEnginesConfigMap}}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;delete

func (r *SystemEnginesReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	declared, err := r.declared(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	holders, err := r.holders(ctx, declared)
	if err != nil {
		return ctrl.Result{}, err
	}
	key := types.NamespacedName{Name: systemEnginesConfigMap, Namespace: r.Namespace}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	found := err == nil
	data, refused := engineSet(existing.Data, declared, holders)
	for engine, by := range refused {
		r.event(existing, found, corev1.EventTypeWarning, "PurgeRefused",
			fmt.Sprintf("the %s engine is not purged: %s still hold it", engine, strings.Join(by, ", ")))
	}
	if !found {
		cm := &corev1.ConfigMap{Data: data}
		cm.Name, cm.Namespace = key.Name, key.Namespace
		cm.Labels = map[string]string{managedByLabel: managedByValue, "gentianos.io/config-type": systemEnginesConfigType}
		if err := r.Create(ctx, cm); err != nil {
			return ctrl.Result{}, err
		}
	} else if !equality.Semantic.DeepEqual(existing.Data, data) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Data = data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels["gentianos.io/config-type"] = systemEnginesConfigType
		if err := r.Patch(ctx, existing, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	// What a purge leaves once the engine's Release is gone: its volumes.
	// A StatefulSet's claims outlive the StatefulSet, which is right for a
	// Release that is upgraded or replaced and wrong for an engine a person
	// asked to have removed.
	pending := false
	for engine, mark := range data {
		if mark != enginePurge {
			continue
		}
		gone, err := r.releaseGone(ctx, engine)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			pending = true
			continue
		}
		if err := r.DeleteAllOf(ctx, &corev1.PersistentVolumeClaim{}, client.InNamespace(engineNamespace(engine))); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("remove the volumes of the purged %s engine: %w", engine, err)
		}
	}
	if pending {
		return ctrl.Result{RequeueAfter: componentRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// engineSet is the set as it must be written, from the set as it is and the
// engines each component declares now, with the purge requests that were
// refused and for whom. It only ever adds "wanted": an engine nothing
// declares any more stays as it was.
//
// A purge is refused while anything holds the engine: a component that
// declares it, or the stores a tenant's record says were made on it for an
// app that is gone -- its data is what a purge of that app, or of the
// tenant, destroys, and those acts must still find the engine to do it.
func engineSet(current map[string]string, declared, holders map[string][]string) (map[string]string, map[string][]string) {
	out := map[string]string{}
	requests := map[string]bool{}
	for k, v := range current {
		if engine, ok := strings.CutPrefix(k, enginePurgeRequestPrefix); ok {
			requests[engine] = true
			continue
		}
		out[k] = v
	}
	refused := map[string][]string{}
	for engine := range requests {
		if by := holders[engine]; len(by) > 0 {
			refused[engine] = by
			continue
		}
		// Only an engine the set holds can be purged; a request for one it
		// never held is dropped with nothing to do.
		if _, held := out[engine]; held {
			out[engine] = enginePurge
		}
	}
	for engine := range declared {
		out[engine] = engineWanted
	}
	return out, refused
}

// declared is, per on-demand engine, the components that declare it now, as
// <namespace>/<name>.
func (r *SystemEnginesReconciler) declared(ctx context.Context) (map[string][]string, error) {
	list := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for i := range list.Items {
		comp := &list.Items[i]
		if comp.DeletionTimestamp != nil {
			continue
		}
		profile, err := getProfileFor(ctx, r.Client, comp.Spec.ProfileRef.Name, comp.Spec.ProfileRef.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for _, engine := range declaredEngines(profile) {
			out[engine] = append(out[engine], comp.Namespace+"/"+comp.Name)
		}
	}
	for engine := range out {
		sort.Strings(out[engine])
	}
	return out, nil
}

// holders is, per on-demand engine, what a purge of it would take data
// from: the components that declare it, and the apps whose stores on it a
// tenant's record names (backup.ReadProvisioned), as <tenant>: <app>
// (recorded).
func (r *SystemEnginesReconciler) holders(ctx context.Context, declared map[string][]string) (map[string][]string, error) {
	out := map[string][]string{}
	for engine, by := range declared {
		out[engine] = append(out[engine], by...)
	}
	records := &corev1.ConfigMapList{}
	if err := r.List(ctx, records, client.InNamespace(layout.Namespace(layout.Provisioning)),
		client.MatchingLabels{backup.ConfigTypeLabel: backup.ProvisionedConfigType}); err != nil {
		return nil, err
	}
	for i := range records.Items {
		record := &records.Items[i]
		entries, err := backup.ReadProvisioned(record)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", record.Name, err)
		}
		tenant := record.Labels[meta.TenantLabel]
		for app, p := range entries {
			who := fmt.Sprintf("%s: %s (recorded)", tenant, app)
			if p.DatabaseEngine == gentianov1alpha1.DatabaseEngineMariaDB {
				out[EngineMariaDB] = append(out[EngineMariaDB], who)
			}
			if p.CacheUser != "" {
				out[EngineCache] = append(out[EngineCache], who)
			}
		}
	}
	for engine := range out {
		sort.Strings(out[engine])
	}
	return out, nil
}

func (r *SystemEnginesReconciler) event(cm *corev1.ConfigMap, found bool, kind, reason, message string) {
	if r.Recorder == nil || !found {
		return
	}
	r.Recorder.Event(cm, kind, reason, message)
}

// releaseGone reports that no Release installs into the engine's namespace
// any more.
func (r *SystemEnginesReconciler) releaseGone(ctx context.Context, engine string) (bool, error) {
	_, found, err := engineRelease(ctx, r.Client, engine)
	return !found, err
}

// engineRelease finds the provider-helm Releases that install an engine
// into its namespace, and reports whether every one is Ready. The Cluster
// composition names Releases after the claim, which the operator does not
// read; the namespace a Release installs into is the engine's alone.
func engineRelease(ctx context.Context, c client.Reader, engine string) (ready, found bool, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseGVK.GroupVersion().WithKind("ReleaseList"))
	if err := c.List(ctx, list); err != nil {
		// A cluster without provider-helm runs no engine of the system
		// tier, and none is ready.
		if apimeta.IsNoMatchError(err) {
			return false, false, nil
		}
		return false, false, err
	}
	ns := engineNamespace(engine)
	ready = true
	for _, rel := range list.Items {
		if target, _, _ := unstructured.NestedString(rel.Object, "spec", "forProvider", "namespace"); target != ns {
			continue
		}
		found = true
		this := false
		conditions, _, _ := unstructured.NestedSlice(rel.Object, "status", "conditions")
		for _, raw := range conditions {
			cond, _ := raw.(map[string]any)
			if cond["type"] == "Ready" && cond["status"] == string(metav1.ConditionTrue) {
				this = true
			}
		}
		ready = ready && this
	}
	return ready && found, found, nil
}

// systemEngineWaiting is the first on-demand engine the profiles declare
// that is not ready yet, "" when there is none, with why it is not when that
// is more than "not yet". A component that declares an engine is installed
// once the engine answers, and waits with a condition naming it until then:
// its first install is what asks for the engine.
//
// The profiles are the component's own and those of the addons it activates:
// an addon runs inside its base, so what it needs, the base waits for.
func systemEngineWaiting(ctx context.Context, c client.Reader, profiles ...*gentianov1alpha1.ComponentProfile) (string, string, error) {
	seen := map[string]bool{}
	for _, profile := range profiles {
		for _, engine := range declaredEngines(profile) {
			if seen[engine] {
				continue
			}
			seen[engine] = true
			ready, why, err := systemEngineReady(ctx, c, engine)
			if err != nil {
				return "", "", err
			}
			if !ready {
				return engine, why, nil
			}
		}
	}
	return "", "", nil
}

// systemEngineReady reports whether an engine answers, and why it cannot
// when it never will on this cluster.
func systemEngineReady(ctx context.Context, c client.Reader, engine string) (bool, string, error) {
	// The TURN edge is no Release: the operator deploys it itself, and only
	// where something can reach it.
	if engine == EngineTURN {
		if !turnReachable() {
			return false, turnUnreachableReason, nil
		}
		ready, err := turnEdgeReady(ctx, c)
		return ready, "", err
	}
	ready, _, err := engineRelease(ctx, c, engine)
	if err != nil {
		return false, "", err
	}
	// The app authorization engine is the instance's Release and the
	// arbiter beside it, which the operator deploys itself.
	if ready && engine == EngineAuthorization {
		if ready, err = arbiterReady(ctx, c); err != nil {
			return false, "", err
		}
	}
	return ready, "", nil
}

func (r *SystemEnginesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toSet := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{systemEnginesRequest}
	})
	ownSet := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == systemEnginesConfigMap && o.GetNamespace() == r.Namespace
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("system-engines").
		Watches(&gentianov1alpha1.Component{}, toSet).
		Watches(&gentianov1alpha1.ComponentProfile{}, toSet).
		Watches(&corev1.ConfigMap{}, toSet, builder.WithPredicates(ownSet)).
		Complete(r.Definitions.Guard(r.Client, &gentianov1alpha1.Component{}, r))
}
