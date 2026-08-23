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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func quiesceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	return s
}

func deployment(name, app string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenant-demo",
			Labels:    map[string]string{"gentianos.io/app": app},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func getDeployment(t *testing.T, c client.Client, name string) *appsv1.Deployment {
	t.Helper()
	got := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "tenant-demo"}, got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got
}

func quiesceReconciler(objs ...client.Object) *TenantReconciler {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &TenantReconciler{Client: c, Scheme: s}
}

// The whole point of the memo: an app comes back at the size it was, not at
// some default that silently rescales a tenant's workload.
func TestQuiesceRemembersReplicaCountAndResumeRestoresIt(t *testing.T) {
	_ = quiesceScheme(t)
	r := quiesceReconciler(deployment("nextcloud", "nextcloud-base-ce", 3))
	ctx := context.Background()

	mode, err := r.quiesceApp(ctx, "demo", "nextcloud-base-ce", nil)
	if err != nil {
		t.Fatalf("quiesceApp: %v", err)
	}
	if mode != gentianov1alpha1.BackupQuiesceScaleDown {
		t.Errorf("mode = %q, want scaleDown", mode)
	}

	paused := getDeployment(t, r.Client, "nextcloud")
	if *paused.Spec.Replicas != 0 {
		t.Errorf("replicas after pause = %d, want 0", *paused.Spec.Replicas)
	}
	if paused.Annotations[replicaMemoAnnotation] != "3" {
		t.Errorf("memo = %q, want 3", paused.Annotations[replicaMemoAnnotation])
	}

	if err := r.resumeApp(ctx, "demo", "nextcloud-base-ce"); err != nil {
		t.Fatalf("resumeApp: %v", err)
	}
	resumed := getDeployment(t, r.Client, "nextcloud")
	if *resumed.Spec.Replicas != 3 {
		t.Errorf("replicas after resume = %d, want 3", *resumed.Spec.Replicas)
	}
	if _, still := resumed.Annotations[replicaMemoAnnotation]; still {
		t.Error("memo survived the resume; a later pause would read a stale count")
	}
}

// Pausing twice is normal — a reconcile can be repeated at any point — and the
// second pass must not overwrite the memo with the paused count of 0, or the
// app resumes to nothing and stays down.
func TestPausingTwiceDoesNotDestroyTheMemo(t *testing.T) {
	r := quiesceReconciler(deployment("nextcloud", "nextcloud-base-ce", 2))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := r.quiesceApp(ctx, "demo", "nextcloud-base-ce", nil); err != nil {
			t.Fatalf("quiesceApp pass %d: %v", i, err)
		}
	}
	if got := getDeployment(t, r.Client, "nextcloud").Annotations[replicaMemoAnnotation]; got != "2" {
		t.Fatalf("memo after repeated pauses = %q, want 2", got)
	}

	if err := r.resumeApp(ctx, "demo", "nextcloud-base-ce"); err != nil {
		t.Fatalf("resumeApp: %v", err)
	}
	if got := *getDeployment(t, r.Client, "nextcloud").Spec.Replicas; got != 2 {
		t.Errorf("replicas after resume = %d, want 2", got)
	}
}

// Resume runs on the failure path and on every reconcile that finds a stale
// entry, so calling it repeatedly, or on a workload that was never paused,
// has to be a no-op rather than a rescale.
func TestResumeIsSafeWhenNothingWasPaused(t *testing.T) {
	r := quiesceReconciler(deployment("nextcloud", "nextcloud-base-ce", 4))
	ctx := context.Background()

	if err := r.resumeApp(ctx, "demo", "nextcloud-base-ce"); err != nil {
		t.Fatalf("resumeApp on unpaused app: %v", err)
	}
	if got := *getDeployment(t, r.Client, "nextcloud").Spec.Replicas; got != 4 {
		t.Errorf("resume rescaled an unpaused workload to %d", got)
	}

	if _, err := r.quiesceApp(ctx, "demo", "nextcloud-base-ce", nil); err != nil {
		t.Fatalf("quiesceApp: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.resumeApp(ctx, "demo", "nextcloud-base-ce"); err != nil {
			t.Fatalf("resumeApp pass %d: %v", i, err)
		}
	}
	if got := *getDeployment(t, r.Client, "nextcloud").Spec.Replicas; got != 4 {
		t.Errorf("replicas after repeated resume = %d, want 4", got)
	}
}

// Pausing one app must not touch another. This is the failure that would be
// reported as "the export took my other app offline".
func TestQuiesceLeavesOtherAppsRunning(t *testing.T) {
	r := quiesceReconciler(
		deployment("nextcloud", "nextcloud-base-ce", 2),
		deployment("openproject", "openproject-ce", 5),
	)
	ctx := context.Background()

	if _, err := r.quiesceApp(ctx, "demo", "nextcloud-base-ce", nil); err != nil {
		t.Fatalf("quiesceApp: %v", err)
	}

	if got := *getDeployment(t, r.Client, "openproject").Spec.Replicas; got != 5 {
		t.Errorf("neighbour was scaled to %d", got)
	}
	if _, memoed := getDeployment(t, r.Client, "openproject").Annotations[replicaMemoAnnotation]; memoed {
		t.Error("neighbour was memoed, so a later resume would rescale it")
	}
}

// A profile asking for command mode still gets its writes paused; only the
// mechanism differs. The returned mode is what reaches the manifest, so it must
// report what actually happened rather than what was requested.
func TestCommandModeFallsBackToScaleDownAndSaysSo(t *testing.T) {
	r := quiesceReconciler(deployment("nextcloud", "nextcloud-base-ce", 1))
	spec := &gentianov1alpha1.BackupSpec{
		Quiesce: &gentianov1alpha1.BackupQuiesce{
			Mode: gentianov1alpha1.BackupQuiesceCommand,
			Pre:  []string{"occ", "maintenance:mode", "--on"},
			Post: []string{"occ", "maintenance:mode", "--off"},
		},
	}

	mode, err := r.quiesceApp(context.Background(), "demo", "nextcloud-base-ce", spec)
	if err != nil {
		t.Fatalf("quiesceApp: %v", err)
	}
	if mode != gentianov1alpha1.BackupQuiesceScaleDown {
		t.Errorf("reported mode = %q, want the mode actually used (scaleDown)", mode)
	}
	if got := *getDeployment(t, r.Client, "nextcloud").Spec.Replicas; got != 0 {
		t.Errorf("writes were not paused: replicas = %d", got)
	}
}

// mode: none means the app is captured live, so nothing may be scaled.
func TestQuiesceNoneLeavesTheAppRunning(t *testing.T) {
	r := quiesceReconciler(deployment("nextcloud", "nextcloud-base-ce", 2))
	spec := &gentianov1alpha1.BackupSpec{
		Quiesce: &gentianov1alpha1.BackupQuiesce{Mode: gentianov1alpha1.BackupQuiesceNone},
	}

	mode, err := r.quiesceApp(context.Background(), "demo", "nextcloud-base-ce", spec)
	if err != nil {
		t.Fatalf("quiesceApp: %v", err)
	}
	if mode != gentianov1alpha1.BackupQuiesceNone {
		t.Errorf("mode = %q, want none", mode)
	}
	if got := *getDeployment(t, r.Client, "nextcloud").Spec.Replicas; got != 2 {
		t.Errorf("mode none scaled the app to %d", got)
	}
}

// A failed capture resumes the app, and the status must say so. Leaving
// quiesceEnd unset on the Failed entry rendered as "paused now" in the Admin
// Console for as long as the failed export existed — an outage display for an
// app that is running again.
func TestFailedCaptureResumesAppAndStampsQuiesceEnd(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	start := metav1.Now()
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "export-x", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Quiesced: []string{"app-store-me"},
			Apps: []gentianov1alpha1.AppExportStatus{{
				Name:         "app-store-me",
				Phase:        gentianov1alpha1.TenantExportPhaseRunning,
				QuiesceStart: &start,
			}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	paused := deployment("app-store-me", "app-store-me", 0)
	paused.Annotations = map[string]string{replicaMemoAnnotation: "2"}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(export, tenant, paused).
		WithStatusSubresource(export).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s,
		Reconciler: &TenantReconciler{Client: c, Scheme: s}}

	if _, err := r.failApp(context.Background(), export, tenant, "app-store-me", "boom"); err != nil {
		t.Fatalf("failApp: %v", err)
	}

	entry := appStatus(&export.Status.Apps, "app-store-me")
	if entry.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Fatalf("app phase = %q, want Failed", entry.Phase)
	}
	if entry.QuiesceEnd == nil {
		t.Fatal("quiesceEnd not set: a resumed app would still display as paused")
	}
	if len(export.Status.Quiesced) != 0 {
		t.Fatalf("status.quiesced = %v, want empty", export.Status.Quiesced)
	}
	if got := *getDeployment(t, c, "app-store-me").Spec.Replicas; got != 2 {
		t.Fatalf("replicas = %d, want the memoed 2", got)
	}
}

// Deleting an export is the only abort a tenant admin has, and it must leave
// nothing behind: paused apps resumed, capture Jobs gone, the bundle removed
// from the object store — and only then the CR itself.
func TestDeletingAnExportResumesAppsCleansBundleThenReleases(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "export-x",
			Namespace:  "tenant-demo",
			Finalizers: []string{exportFinalizer},
		},
		Status: gentianov1alpha1.TenantExportStatus{
			Phase:    gentianov1alpha1.TenantExportPhaseRunning,
			Quiesced: []string{"app-store-me"},
			Bundle: &gentianov1alpha1.BundleRef{
				Bucket: "demo-gentian-backup",
				Prefix: "export-x",
			},
		},
	}
	paused := deployment("app-store-me", "app-store-me", 0)
	paused.Annotations = map[string]string{replicaMemoAnnotation: "1"}
	captureJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "tx-export-x-app-store-me-pg",
		Namespace: kernelNamespace,
		Labels:    map[string]string{backup.ExportLabel: "export-x"},
	}}
	// A live namespace and tenant: this is someone deleting one backup, which
	// is the only case that may remove a bundle.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}}
	liveTenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(export, paused, captureJob, ns, liveTenant).
		WithStatusSubresource(export).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s,
		Reconciler: &TenantReconciler{Client: c, Scheme: s}}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "export-x", Namespace: "tenant-demo"}}

	if err := c.Delete(ctx, export); err != nil {
		t.Fatalf("delete export: %v", err)
	}

	// First pass: resume the app, kill the capture Job, start the cleanup Job.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := *getDeployment(t, c, "app-store-me").Spec.Replicas; got != 1 {
		t.Fatalf("replicas = %d, want resumed 1", got)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: captureJob.Name, Namespace: kernelNamespace}, &batchv1.Job{}); err == nil {
		t.Fatal("capture Job survived deletion; it would upload into the prefix being removed")
	}
	cleanup := &batchv1.Job{}
	cleanupName := bundleDeleteJobName("export-x")
	if err := c.Get(ctx, types.NamespacedName{Name: cleanupName, Namespace: kernelNamespace}, cleanup); err != nil {
		t.Fatalf("cleanup Job not created: %v", err)
	}

	// The CR must survive until the bundle is actually gone.
	if err := c.Get(ctx, req.NamespacedName, &gentianov1alpha1.TenantExport{}); err != nil {
		t.Fatalf("export released before the bundle was removed: %v", err)
	}

	cleanup.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	if err := c.Status().Update(ctx, cleanup); err != nil {
		t.Fatalf("mark cleanup complete: %v", err)
	}

	// Second pass: cleanup done, finalizer removed, CR gone.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &gentianov1alpha1.TenantExport{}); !apierrors.IsNotFound(err) {
		t.Fatalf("export still present after cleanup completed: %v", err)
	}
}

// The status is the record that a unit finished — the Job is not. The kernel
// Job GC and the Job's own TTL can both remove a completed Job while a
// sibling unit still runs, and recreating it re-ran a dump that had already
// been uploaded.
func TestCompletedUnitIsNotRerunAfterItsJobDisappears(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	const jobName = "tx-export-x-app-store-me-pg"
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "export-x", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Apps: []gentianov1alpha1.AppExportStatus{{
				Name:           "app-store-me",
				CompletedUnits: []string{jobName},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(export).
		WithStatusSubresource(export).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s,
		Reconciler: &TenantReconciler{Client: c, Scheme: s}}

	unit := captureUnit{JobName: jobName, Job: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      jobName,
		Namespace: kernelNamespace,
		Labels:    map[string]string{meta.AppLabel: "app-store-me"},
	}}}
	done, err := r.ensureCaptureJob(context.Background(), export, unit)
	if err != nil {
		t.Fatalf("ensureCaptureJob: %v", err)
	}
	if !done {
		t.Fatal("recorded-complete unit reported not done")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the Job was recreated for an already-completed unit: %v", err)
	}
}

// A PVC is only mountable from its own namespace. The first live volume
// capture ran in the kernel namespace and sat Pending forever — holding its
// app paused — because the claim it named exists only in the tenant
// namespace. Volume units must therefore run where the claim lives, with the
// staged credential copy, while every other unit stays in the kernel
// namespace where the admin secrets are.
func TestVolumeUnitsRunInTheTenantNamespaceWithStagedCredentials(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "nextcloud-nextcloud",
		Namespace: "tenant-demo",
		Labels:    map[string]string{"gentianos.io/app": "nextcloud-base-ce"},
	}}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce"},
		Spec:       gentianov1alpha1.AppProfileSpec{Family: "nextcloud"},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(tenant, export, pvc).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s,
		Reconciler: &TenantReconciler{Client: c, Scheme: s}}

	enc := backup.Encryption{
		Mode:             gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecret: "tx-nightly-passphrase",
		PassphraseKey:    "passphrase",
	}
	units := r.captureUnits(context.Background(), tenant, "nextcloud-base-ce", profile, export, enc)

	var volume *captureUnit
	for i := range units {
		u := &units[i]
		if u.Kind == "volume" {
			volume = u
		} else if u.Job.Namespace != kernelNamespace {
			t.Errorf("%s unit runs in %q, want the kernel namespace", u.Kind, u.Job.Namespace)
		}
	}
	if volume == nil {
		t.Fatal("no volume unit for a claim the app owns")
	}
	if volume.Job.Namespace != "tenant-demo" {
		t.Fatalf("volume Job namespace = %q, want tenant-demo", volume.Job.Namespace)
	}
	staged := volumeUploadSecretName(export.Name)
	var minioFromStaged, passphraseFromStaged bool
	containers := append(append([]corev1.Container{},
		volume.Job.Spec.Template.Spec.InitContainers...),
		volume.Job.Spec.Template.Spec.Containers...)
	for _, container := range containers {
		for _, env := range container.Env {
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			from := env.ValueFrom.SecretKeyRef.Name
			if env.Name == "MINIO_ENDPOINT" && from == staged {
				minioFromStaged = true
			}
			if env.Name == backup.PassphraseEnvVar && from == staged {
				passphraseFromStaged = true
			}
		}
	}
	if !minioFromStaged {
		t.Error("volume Job does not read MinIO credentials from the staged copy")
	}
	if !passphraseFromStaged {
		t.Error("volume Job does not read the passphrase from the staged copy")
	}
}

type fakeExecer struct {
	calls [][]string
}

func (f *fakeExecer) Exec(_ context.Context, _, _, _ string, argv []string) (string, error) {
	f.calls = append(f.calls, argv)
	return "", nil
}

// An app paused by its maintenance hook must be resumed by its resume hook.
// The export used to scale-restore only — a no-op for command mode — and the
// first live command-mode capture left Nextcloud serving its maintenance page
// forever while the export status said resumed.
func TestCommandModeResumeRunsTheResumeHook(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "nextcloud-base-ce"}},
		},
	}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Backup: &gentianov1alpha1.BackupSpec{
				Quiesce: &gentianov1alpha1.BackupQuiesce{
					Mode: gentianov1alpha1.BackupQuiesceCommand,
					Pre:  []string{"php", "occ", "maintenance:mode", "--on"},
					Post: []string{"php", "occ", "maintenance:mode", "--off"},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nextcloud-abc",
			Namespace: "tenant-demo",
			Labels:    map[string]string{"gentianos.io/app": "nextcloud-base-ce"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nextcloud"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(tenant, profile, pod).Build()
	execer := &fakeExecer{}
	tr := &TenantReconciler{Client: c, Scheme: s, Exec: execer}

	if err := resumeQuiescedApp(context.Background(), c, tr,
		"demo", "nextcloud-base-ce", string(gentianov1alpha1.BackupQuiesceCommand), ""); err != nil {
		t.Fatalf("resumeQuiescedApp: %v", err)
	}

	found := false
	for _, argv := range execer.calls {
		if len(argv) == 4 && argv[3] == "--off" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resume hook never ran; calls: %v — the app stays in maintenance mode", execer.calls)
	}
}

// The hook must land in the pod that actually has its target container. The
// loose name match also catches an app's sidecar releases, and the first live
// post-restore hook ran in the MCP sidecar's pod — the real pod was briefly
// not Ready, and unreadiness is the normal state right after a restore, since
// the hook is often the thing that makes the app ready again.
func TestHookPodSelectionPrefersTheTargetContainer(t *testing.T) {
	s := quiesceScheme(t)

	sidecar := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nextcloud-base-ce-mcp-release-abc",
			Namespace: "tenant-demo",
			Labels:    map[string]string{"gentianos.io/app": "nextcloud-base-ce"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "mcp"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	// The real pod: right container, NOT Ready — the post-restore state.
	main := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nextcloud-xyz",
			Namespace: "tenant-demo",
			Labels:    map[string]string{"gentianos.io/app": "nextcloud-base-ce"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nextcloud"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sidecar, main).Build()
	r := &TenantReconciler{Client: c, Scheme: s}

	pod, err := r.runningPodForApp(context.Background(), "demo", "nextcloud-base-ce", "nextcloud")
	if err != nil {
		t.Fatalf("runningPodForApp: %v", err)
	}
	if pod.Name != "nextcloud-xyz" {
		t.Fatalf("picked %q — a pod without the hook's container", pod.Name)
	}
}

// A scale-down quiesce leaves no pod, so a post-restore hook cannot run — the
// first live restore of an app whose maintenance command was unavailable
// failed with "no running pod" after writing all its data correctly. The app
// must be resumed before its hooks, not after.
func TestScaleDownRestoreResumesBeforeRunningHooks(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	spec := &gentianov1alpha1.BackupSpec{
		Restore: &gentianov1alpha1.BackupRestore{
			Post: [][]string{{"php", "occ", "maintenance:data-fingerprint"}},
		},
	}
	if !restoreHooksNeedPod(spec) {
		t.Fatal("a profile with restore.post must be reported as needing a pod")
	}
	if restoreHooksNeedPod(&gentianov1alpha1.BackupSpec{}) {
		t.Error("a profile with no restore hooks must not wait for a pod")
	}

	// Scaled to zero, as a scale-down quiesce leaves it: no pod exists, so a
	// hook could not run until the app is scaled back up.
	scaled := deployment("nextcloud", "nextcloud-base-ce", 0)
	scaled.Annotations = map[string]string{replicaMemoAnnotation: "1"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(scaled).Build()
	tr := &TenantReconciler{Client: c, Scheme: s}

	if _, err := tr.runningPodForApp(context.Background(), "demo", "nextcloud-base-ce", "nextcloud"); err == nil {
		t.Fatal("expected no pod while scaled to zero — the premise of the bug")
	}
	if err := tr.unquiesceApp(context.Background(), "demo", "nextcloud-base-ce", spec,
		gentianov1alpha1.BackupQuiesceScaleDown); err != nil {
		t.Fatalf("unquiesceApp: %v", err)
	}
	if got := *getDeployment(t, c, "nextcloud").Spec.Replicas; got != 1 {
		t.Fatalf("replicas = %d after resume-before-hooks, want the memoed 1", got)
	}
}

// A backup must outlive the tenant it protects. These CRs live in the tenant's
// namespace, so a teardown deletes them — and deleting them used to delete
// their bundles, which made "purge the tenant, then restore it" destroy the
// only thing that could have restored it.
func TestTenantTeardownKeepsTheBundle(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}

	newExport := func(name string) *gentianov1alpha1.TenantExport {
		return &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "tenant-demo",
				Finalizers: []string{exportFinalizer},
			},
			Status: gentianov1alpha1.TenantExportStatus{
				Phase: gentianov1alpha1.TenantExportPhaseReady,
				Bundle: &gentianov1alpha1.BundleRef{
					Bucket: "demo-gentian-backup", Prefix: name,
				},
			},
		}
	}

	terminating := metav1.Now()
	for _, tc := range []struct {
		name    string
		objects []client.Object
	}{
		{
			name: "namespace terminating",
			objects: []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name:              "tenant-demo",
					DeletionTimestamp: &terminating,
					Finalizers:        []string{"kubernetes"},
				}},
				&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}},
			},
		},
		{
			name: "tenant terminating",
			objects: []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}},
				&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{
					Name:              "demo",
					DeletionTimestamp: &terminating,
					Finalizers:        []string{tenantFinalizer},
				}},
			},
		},
		{
			name: "tenant already gone",
			objects: []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			export := newExport("keep-me")
			objs := append([]client.Object{export}, tc.objects...)
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(objs...).WithStatusSubresource(export).Build()
			r := &TenantExportReconciler{Client: c, Scheme: s,
				Reconciler: &TenantReconciler{Client: c, Scheme: s}}
			ctx := context.Background()

			if err := c.Delete(ctx, export); err != nil {
				t.Fatalf("delete export: %v", err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
				Name: "keep-me", Namespace: "tenant-demo"}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			// No cleanup Job: the bundle's objects stay in the bucket.
			if err := c.Get(ctx, types.NamespacedName{
				Name: bundleDeleteJobName("keep-me"), Namespace: kernelNamespace,
			}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
				t.Fatalf("a cleanup Job was created during teardown: %v", err)
			}
			// And the CR is released rather than wedged behind its finalizer,
			// which would block the namespace from ever finishing deletion.
			if err := c.Get(ctx, types.NamespacedName{
				Name: "keep-me", Namespace: "tenant-demo",
			}, &gentianov1alpha1.TenantExport{}); !apierrors.IsNotFound(err) {
				t.Fatalf("export still held by its finalizer during teardown: %v", err)
			}
		})
	}
}
