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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// sleepWorld is a tenant with something of every kind in its own namespace
// and its DMZ, beside a namespace that is another tenant's.
type sleepWorld struct {
	r      *TenantReconciler
	tenant *gentianov1alpha1.Tenant
}

func tenantNS(name, tenant string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{layout.LabelTenant: tenant}}}
}

func workloadIn(ns, name string, replicas int32) *appsv1.Deployment {
	d := deployment(name, name, replicas)
	d.Namespace = ns
	return d
}

func newSleepWorld(t *testing.T, hibernation *gentianov1alpha1.TenantHibernation, extra ...client.Object) *sleepWorld {
	t.Helper()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{DisplayName: "Demo", Hibernation: hibernation},
	}
	two, yes, no := int32(2), true, false
	objs := []client.Object{
		tenant,
		tenantNS("tenant-demo", "demo"), tenantNS("tenant-demo-dmz", "demo"), tenantNS("tenant-other", "other"),
		workloadIn("tenant-demo", "nextcloud", 3),
		workloadIn("tenant-demo", "parked", 0),
		workloadIn("tenant-demo-dmz", "perimeter-shares", 1),
		workloadIn("tenant-other", "nextcloud", 1),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "open-webui", Namespace: "tenant-demo"},
			Spec: appsv1.StatefulSetSpec{Replicas: &two}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
			Spec: batchv1.CronJobSpec{Schedule: "0 3 * * *", Suspend: &no}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "switched-off", Namespace: "tenant-demo"},
			Spec: batchv1.CronJobSpec{Schedule: "0 3 * * *", Suspend: &yes}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-abc", Namespace: "tenant-demo"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-done", Namespace: "tenant-demo"},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: "tenant-other"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	}
	objs = append(objs, extra...)
	s := quiesceScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&gentianov1alpha1.Tenant{}, &gentianov1alpha1.TenantExport{}).
		WithObjects(objs...).Build()
	return &sleepWorld{r: &TenantReconciler{Client: c, Scheme: s}, tenant: tenant}
}

func (w *sleepWorld) step(t *testing.T) hibernationStep {
	t.Helper()
	ctx := context.Background()
	if err := w.r.Get(ctx, client.ObjectKeyFromObject(w.tenant), w.tenant); err != nil {
		t.Fatal(err)
	}
	step, err := w.r.reconcileHibernation(ctx, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func (w *sleepWorld) setHibernation(t *testing.T, h *gentianov1alpha1.TenantHibernation) {
	t.Helper()
	ctx := context.Background()
	if err := w.r.Get(ctx, client.ObjectKeyFromObject(w.tenant), w.tenant); err != nil {
		t.Fatal(err)
	}
	w.tenant.Spec.Hibernation = h
	if err := w.r.Update(ctx, w.tenant); err != nil {
		t.Fatal(err)
	}
}

func (w *sleepWorld) replicas(t *testing.T, ns, name string) int32 {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, d); err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func (w *sleepWorld) closed(t *testing.T, ns string) bool {
	t.Helper()
	quota := &corev1.ResourceQuota{}
	err := w.r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: hibernationQuotaName}, quota)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	pods := quota.Spec.Hard[corev1.ResourcePods]
	return pods.IsZero() && len(quota.Spec.Hard) == 1
}

func (w *sleepWorld) exists(t *testing.T, obj client.Object) bool {
	t.Helper()
	err := w.r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return err == nil
}

func (w *sleepWorld) suspended(t *testing.T, name string) bool {
	t.Helper()
	cj := &batchv1.CronJob{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Namespace: "tenant-demo", Name: name}, cj); err != nil {
		t.Fatal(err)
	}
	return cj.Spec.Suspend != nil && *cj.Spec.Suspend
}

func pod(ns, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

// A tenant that is asleep has no pod in its namespaces and can be given none,
// says so, and nothing of another tenant is touched.
func TestASleepingTenantRunsNothingAndAdmitsNothing(t *testing.T) {
	w := newSleepWorld(t, &gentianov1alpha1.TenantHibernation{Asleep: true})
	step := w.step(t)
	if !step.asleep || step.within != hibernationRecheck {
		t.Fatalf("step = %+v, want the reconcile ended and a look again in %s", step, hibernationRecheck)
	}
	for _, ns := range []string{"tenant-demo", "tenant-demo-dmz"} {
		if !w.closed(t, ns) {
			t.Errorf("%s admits pods", ns)
		}
	}
	if w.closed(t, "tenant-other") {
		t.Error("another tenant's namespace was closed")
	}
	if got := w.replicas(t, "tenant-demo", "nextcloud"); got != 0 {
		t.Errorf("nextcloud has %d replicas", got)
	}
	if got := w.replicas(t, "tenant-demo-dmz", "perimeter-shares"); got != 0 {
		t.Errorf("the perimeter proxy has %d replicas", got)
	}
	if got := w.replicas(t, "tenant-other", "nextcloud"); got != 1 {
		t.Errorf("another tenant's app has %d replicas", got)
	}
	st := &appsv1.StatefulSet{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Namespace: "tenant-demo", Name: "open-webui"}, st); err != nil {
		t.Fatal(err)
	}
	if *st.Spec.Replicas != 0 || st.Annotations[hibernationReplicasAnnotation] != "2" {
		t.Errorf("the statefulset has %d replicas and keeps %q", *st.Spec.Replicas, st.Annotations[hibernationReplicasAnnotation])
	}
	if !w.suspended(t, "nightly") {
		t.Error("the CronJob still runs")
	}
	if w.exists(t, pod("tenant-demo", "job-abc")) {
		t.Error("a running pod was left")
	}
	if !w.exists(t, pod("tenant-demo", "job-done")) {
		t.Error("a pod that had ended was deleted")
	}
	if !w.exists(t, pod("tenant-other", "theirs")) {
		t.Error("another tenant's pod was deleted")
	}
	if w.tenant.Status.Phase != gentianov1alpha1.TenantPhaseHibernated || !sleeps(w.tenant) ||
		!apimeta.IsStatusConditionTrue(w.tenant.Status.Conditions, conditionHibernated) {
		t.Errorf("status does not say asleep: %+v", w.tenant.Status)
	}
}

// Waking puts back exactly what sleep took: the replicas each workload had,
// the CronJobs that ran, and the room for pods. What was at zero or switched
// off before stays so.
func TestWakingPutsBackWhatSleepTook(t *testing.T) {
	w := newSleepWorld(t, &gentianov1alpha1.TenantHibernation{Asleep: true})
	w.step(t)
	since := w.tenant.Status.Hibernation.Since

	w.setHibernation(t, nil)
	step := w.step(t)
	if step.asleep || step.within != 0 {
		t.Fatalf("step = %+v, want the reconcile to go on", step)
	}
	for _, ns := range []string{"tenant-demo", "tenant-demo-dmz"} {
		if w.exists(t, &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: hibernationQuotaName}}) {
			t.Errorf("%s still admits no pods", ns)
		}
	}
	for name, want := range map[string]int32{"nextcloud": 3, "parked": 0} {
		if got := w.replicas(t, "tenant-demo", name); got != want {
			t.Errorf("%s has %d replicas, had %d", name, got, want)
		}
	}
	if got := w.replicas(t, "tenant-demo-dmz", "perimeter-shares"); got != 1 {
		t.Errorf("the perimeter proxy has %d replicas, had 1", got)
	}
	d := &appsv1.Deployment{}
	if err := w.r.Get(context.Background(), types.NamespacedName{Namespace: "tenant-demo", Name: "nextcloud"}, d); err != nil {
		t.Fatal(err)
	}
	if _, kept := d.Annotations[hibernationReplicasAnnotation]; kept {
		t.Error("the replicas are still kept on a workload that has them back")
	}
	if w.suspended(t, "nightly") {
		t.Error("the CronJob was not resumed")
	}
	if !w.suspended(t, "switched-off") {
		t.Error("a CronJob that was switched off before runs now")
	}
	if sleeps(w.tenant) || w.tenant.Status.Hibernation == nil || w.tenant.Status.Hibernation.Since == nil ||
		w.tenant.Status.Hibernation.Since.Before(since) ||
		apimeta.IsStatusConditionTrue(w.tenant.Status.Conditions, conditionHibernated) {
		t.Errorf("status does not say woken: %+v", w.tenant.Status)
	}
	// Awake and never asleep again: nothing more to do.
	if step := w.step(t); step != (hibernationStep{}) {
		t.Errorf("an awake tenant asks for %+v", step)
	}
}

// A tenant kept awake until a time is looked at again when it has come, and
// sleeps then with nothing written to it.
func TestATenantKeptAwakeSleepsWhenTheTimeHasCome(t *testing.T) {
	until := metav1.NewTime(time.Now().Add(time.Hour))
	w := newSleepWorld(t, &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &until})
	step := w.step(t)
	if step.asleep || step.within <= 59*time.Minute || step.within > time.Hour+time.Second {
		t.Fatalf("step = %+v, want a look again in an hour", step)
	}
	if w.closed(t, "tenant-demo") || w.replicas(t, "tenant-demo", "nextcloud") != 3 {
		t.Fatal("a tenant kept awake was put to sleep")
	}

	past := metav1.NewTime(time.Now().Add(-time.Minute))
	w.setHibernation(t, &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &past})
	if step := w.step(t); !step.asleep {
		t.Fatalf("step = %+v, want asleep", step)
	}
	if !w.closed(t, "tenant-demo") || w.replicas(t, "tenant-demo", "nextcloud") != 0 {
		t.Fatal("the tenant did not sleep when its time had come")
	}

	// Woken for a while: awake now, and looked at again at the new time.
	w.setHibernation(t, &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &until})
	step = w.step(t)
	if step.asleep || step.within == 0 || w.replicas(t, "tenant-demo", "nextcloud") != 3 {
		t.Fatalf("step = %+v with %d replicas, want awake and a look again", step, w.replicas(t, "tenant-demo", "nextcloud"))
	}
}

// awakeUntil without asleep says nothing, and the platform tenant never
// sleeps: the cluster is administered through it.
func TestWhoSleeps(t *testing.T) {
	now := time.Now()
	past, future := metav1.NewTime(now.Add(-time.Hour)), metav1.NewTime(now.Add(time.Hour))
	for name, c := range map[string]struct {
		tenant string
		h      *gentianov1alpha1.TenantHibernation
		want   bool
	}{
		"nothing said":                {"demo", nil, false},
		"asleep":                      {"demo", &gentianov1alpha1.TenantHibernation{Asleep: true}, true},
		"asleep, kept awake":          {"demo", &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &future}, false},
		"asleep, kept awake until":    {"demo", &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &past}, true},
		"a time and not asleep":       {"demo", &gentianov1alpha1.TenantHibernation{AwakeUntil: &past}, false},
		"the platform tenant, asleep": {gentianov1alpha1.PlatformTenantName, &gentianov1alpha1.TenantHibernation{Asleep: true}, false},
	} {
		tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: c.tenant},
			Spec: gentianov1alpha1.TenantSpec{Hibernation: c.h}}
		if got := tenant.Asleep(now); got != c.want {
			t.Errorf("%s: asleep = %v, want %v", name, got, c.want)
		}
	}
}

// What something scaled up while the tenant slept is put back to zero, and
// comes back at the count it was given.
func TestAWorkloadScaledUpInSleepIsPutBackToZero(t *testing.T) {
	w := newSleepWorld(t, &gentianov1alpha1.TenantHibernation{Asleep: true})
	w.step(t)
	ctx := context.Background()
	d := &appsv1.Deployment{}
	if err := w.r.Get(ctx, types.NamespacedName{Namespace: "tenant-demo", Name: "nextcloud"}, d); err != nil {
		t.Fatal(err)
	}
	five := int32(5)
	d.Spec.Replicas = &five
	if err := w.r.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	w.step(t)
	if got := w.replicas(t, "tenant-demo", "nextcloud"); got != 0 {
		t.Fatalf("nextcloud has %d replicas in a sleeping tenant", got)
	}
	w.setHibernation(t, nil)
	w.step(t)
	if got := w.replicas(t, "tenant-demo", "nextcloud"); got != 5 {
		t.Fatalf("nextcloud came back with %d replicas, was given 5", got)
	}
}

// An export scales apps down and up again. A tenant with one running is not
// put to sleep until it has ended, and is reconciled as awake meanwhile.
func TestATenantSleepsOnlyWhenItsExportHasEnded(t *testing.T) {
	export := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"}}
	w := newSleepWorld(t, &gentianov1alpha1.TenantHibernation{Asleep: true}, export)
	ctx := context.Background()
	export.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
	if err := w.r.Status().Update(ctx, export); err != nil {
		t.Fatal(err)
	}
	step := w.step(t)
	if step.asleep || step.within != hibernationBusyRecheck {
		t.Fatalf("step = %+v, want awake and a look again in %s", step, hibernationBusyRecheck)
	}
	if w.closed(t, "tenant-demo") || sleeps(w.tenant) {
		t.Fatal("put to sleep under a running export")
	}
	cond := apimeta.FindStatusCondition(w.tenant.Status.Conditions, conditionHibernated)
	if cond == nil || cond.Reason != "WaitingForBackup" {
		t.Fatalf("the status does not say what is waited for: %+v", cond)
	}

	export.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	if err := w.r.Status().Update(ctx, export); err != nil {
		t.Fatal(err)
	}
	if step := w.step(t); !step.asleep || !w.closed(t, "tenant-demo") {
		t.Fatalf("step = %+v: not asleep after the export ended", step)
	}
}

// The edge answers for every route of a sleeping tenant's apps, with the
// answer of a tenant that is asleep, and nothing of the component is
// reconciled. Woken, the routes stay held until the app's pods are back, and
// no longer than hibernationWakeHold.
func TestTheEdgeAnswersForASleepingTenant(t *testing.T) {
	w := newHoldWorld(t)
	ctx := context.Background()
	setSleep := func(status *gentianov1alpha1.TenantHibernationStatus) {
		t.Helper()
		tenant := &gentianov1alpha1.Tenant{}
		if err := w.c.Get(ctx, client.ObjectKeyFromObject(w.tenant), tenant); err != nil {
			t.Fatal(err)
		}
		tenant.Status.Hibernation = status
		if err := w.c.Update(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	hold := func() *sleepWait {
		t.Helper()
		held, wait, err := w.r.restoreHold(ctx, w.comp)
		if err != nil {
			t.Fatal(err)
		}
		if held {
			t.Fatal("held for a restore there is none of")
		}
		return wait
	}
	heldBy := func(key types.NamespacedName) string { return routeHeldBy(w.route(t, key)) }

	now := metav1.Now()
	setSleep(&gentianov1alpha1.TenantHibernationStatus{Asleep: true, Since: &now})
	if wait := hold(); wait == nil || !wait.asleep {
		t.Fatalf("wait = %+v, want asleep", wait)
	}
	for _, key := range []types.NamespacedName{w.entry, w.surface} {
		if got := heldBy(key); got != hibernationHoldFilterName {
			t.Fatalf("%s is held by %q", key, got)
		}
		filter := &unstructured.Unstructured{}
		filter.SetGroupVersionKind(httpRouteFilterGVK)
		if err := w.c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: hibernationHoldFilterName}, filter); err != nil {
			t.Fatalf("no answer in %s: %v", key.Namespace, err)
		}
		if body, _, _ := unstructured.NestedString(filter.Object, "spec", "directResponse", "body", "inline"); body != hibernationHoldBody {
			t.Errorf("the answer in %s is %q", key.Namespace, body)
		}
	}
	w.assertOpen(t, w.other)

	// Woken, and the app's pods are not back.
	one := int32(1)
	app := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud", Namespace: w.comp.Namespace,
			Labels: map[string]string{"gentianos.io/app": w.comp.Spec.ProfileRef.Name}},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
	}
	if err := w.c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	setSleep(&gentianov1alpha1.TenantHibernationStatus{Since: &now})
	if wait := hold(); wait == nil || wait.asleep || len(wait.pending) != 1 {
		t.Fatalf("wait = %+v, want the deployment waited for", wait)
	}
	if got := heldBy(w.entry); got != hibernationHoldFilterName {
		t.Fatalf("the entry is held by %q while its pods are not back", got)
	}

	// Its pods are back.
	app.Status = appsv1.DeploymentStatus{ObservedGeneration: app.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if err := w.c.Status().Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	if wait := hold(); wait != nil {
		t.Fatalf("wait = %+v with the pods back", wait)
	}
	w.assertOpen(t, w.entry)
	w.assertOpen(t, w.surface)
}

// An app that does not come back is not kept behind the answer of a sleeping
// tenant for good.
func TestAWokenAppThatDoesNotStartIsServedAfterAWhile(t *testing.T) {
	w := newHoldWorld(t)
	ctx := context.Background()
	if err := w.r.applyHold(ctx, w.comp, layout.TenantDMZ(w.tenant.Name), hibernationHoldFilterName); err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	if err := w.c.Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud", Namespace: w.comp.Namespace,
			Labels: map[string]string{"gentianos.io/app": w.comp.Spec.ProfileRef.Name}},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
	}); err != nil {
		t.Fatal(err)
	}
	tenant := &gentianov1alpha1.Tenant{}
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(w.tenant), tenant); err != nil {
		t.Fatal(err)
	}
	long := metav1.NewTime(time.Now().Add(-hibernationWakeHold - time.Minute))
	tenant.Status.Hibernation = &gentianov1alpha1.TenantHibernationStatus{Since: &long}
	if err := w.c.Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, wait, err := w.r.restoreHold(ctx, w.comp); err != nil || wait != nil {
		t.Fatalf("wait = %+v, err = %v", wait, err)
	}
	w.assertOpen(t, w.entry)
}

// Components are told when the operator puts their tenant to sleep or wakes
// it, and by nothing else that changes on a tenant.
func TestOnlySleepAndWakingTellTheComponents(t *testing.T) {
	asleep := &gentianov1alpha1.Tenant{Status: gentianov1alpha1.TenantStatus{
		Hibernation: &gentianov1alpha1.TenantHibernationStatus{Asleep: true}}}
	awake := &gentianov1alpha1.Tenant{}
	other := awake.DeepCopy()
	other.Status.Phase = gentianov1alpha1.TenantPhaseReady
	p := sleepChanged()
	if !p.Update(event.UpdateEvent{ObjectOld: awake, ObjectNew: asleep}) || !p.Update(event.UpdateEvent{ObjectOld: asleep, ObjectNew: awake}) {
		t.Error("going to sleep or waking tells nobody")
	}
	if p.Update(event.UpdateEvent{ObjectOld: awake, ObjectNew: other}) {
		t.Error("a change that is not sleep tells the components")
	}
}

// A sleeping tenant starts no run: neither it nor its callers are in the
// dispatcher's table, and they are again when it has been woken.
func TestASleepingTenantIsNotInTheDispatchersTable(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	tenant := rightsTenant("acme")
	tenant.Status.Hibernation = &gentianov1alpha1.TenantHibernationStatus{Asleep: true}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "tenant-acme"}}
	c, r := runsHarness(t, tenant, flow, runsProfile("flow"), service)
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	if table := runsTable(t, c); len(table.Tenants) != 0 || len(table.Callers) != 0 {
		t.Fatalf("a sleeping tenant is in the table: %+v", table)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(tenant), tenant); err != nil {
		t.Fatal(err)
	}
	tenant.Status.Hibernation = nil
	if err := c.Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	if table := runsTable(t, c); len(table.Tenants) != 1 || len(table.Callers) != 1 {
		t.Fatalf("a woken tenant is not in the table: %+v", table)
	}
}

// An export of a sleeping tenant is skipped, not failed: nothing is captured,
// it has no bundle, and the record says the tenant is asleep. One that had
// its bundle before the tenant slept is left to finish.
func TestAnExportOfASleepingTenantIsSkipped(t *testing.T) {
	s := quiesceScheme(t)
	ctx := context.Background()
	asleep := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{Hibernation: &gentianov1alpha1.TenantHibernation{Asleep: true}}}
	later := metav1.NewTime(time.Now().Add(time.Hour))
	keptAwake := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "awake"},
		Spec: gentianov1alpha1.TenantSpec{Hibernation: &gentianov1alpha1.TenantHibernation{Asleep: true, AwakeUntil: &later}}}
	export := func(ns string) *gentianov1alpha1.TenantExport {
		return &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: ns, Finalizers: []string{exportFinalizer}}}
	}
	skipped, taken := export("tenant-demo"), export("tenant-awake")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(asleep, keptAwake, skipped, taken).
		WithStatusSubresource(skipped, taken).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s, Reconciler: &TenantReconciler{Client: c, Scheme: s}}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(skipped)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.TenantExport{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(skipped), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseSkipped || !got.IsTerminal() || got.Status.Bundle != nil || len(got.Status.Apps) != 0 {
		t.Fatalf("status = %+v, want skipped with nothing captured", got.Status)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionExportComplete)
	if cond == nil || cond.Reason != reasonTenantAsleep || !strings.Contains(cond.Message, "asleep") {
		t.Fatalf("the record does not say why: %+v", cond)
	}

	// A tenant that is kept awake is exported like any other: its export is
	// not ended here, whatever else it then waits for.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(taken)})
	if err := c.Get(ctx, client.ObjectKeyFromObject(taken), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == gentianov1alpha1.TenantExportPhaseSkipped {
		t.Fatal("an export of a tenant that is kept awake was skipped")
	}
}

// Falling asleep and waking go into the tenant's usage history once each, at
// the time the operator did it and with who asked; a tenant that fell asleep
// because its time had come has nobody to name; and a history that cannot be
// written is written on a later look.
func TestSleepAndWakingAreRecordedInTheUsageHistory(t *testing.T) {
	ctx := context.Background()
	since := metav1.NewTime(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC))
	tenant := func(asleep bool, until *metav1.Time) *gentianov1alpha1.Tenant {
		return &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec: gentianov1alpha1.TenantSpec{Hibernation: &gentianov1alpha1.TenantHibernation{
				Asleep: true, AwakeUntil: until, SetBy: "tom@example.com",
			}},
			Status: gentianov1alpha1.TenantStatus{Hibernation: &gentianov1alpha1.TenantHibernationStatus{
				Asleep: asleep, Since: &since,
			}},
		}
	}
	later := metav1.NewTime(since.Add(72 * time.Hour))

	store := &recordedPlans{}
	r := planReconciler(t, store)

	put := tenant(true, nil)
	if !r.recordHibernation(ctx, put) || !put.Status.Hibernation.Recorded {
		t.Fatal("a tenant put to sleep was not recorded")
	}
	if r.recordHibernation(ctx, put) {
		t.Fatal("the same change was recorded twice")
	}
	woken := tenant(false, &later)
	r.recordHibernation(ctx, woken)
	fell := tenant(true, &since)
	r.recordHibernation(ctx, fell)

	want := []usage.HibernationEvent{
		{OccurredAt: since.UTC(), Asleep: true, Actor: "tom@example.com"},
		{OccurredAt: since.UTC(), Asleep: false, Actor: "tom@example.com"},
		{OccurredAt: since.UTC(), Asleep: true},
	}
	if len(store.sleep) != len(want) {
		t.Fatalf("events = %+v", store.sleep)
	}
	for i := range want {
		if store.sleep[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, store.sleep[i], want[i])
		}
	}

	// No database yet: nothing is noted, so the next reconcile tries again.
	unreachable := tenant(true, nil)
	if planReconciler(t, nil).recordHibernation(ctx, unreachable) || unreachable.Status.Hibernation.Recorded {
		t.Fatal("a change that was not written was noted as recorded")
	}
	// A tenant that never slept has nothing to record.
	if r.recordHibernation(ctx, &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}) {
		t.Fatal("a tenant that never slept was recorded")
	}
}
