/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// fakeVault is the vault as a purge sees it: paths, and what was deleted.
type fakeVault struct {
	mu      sync.Mutex
	paths   map[string]bool
	deleted []string
	// fail makes DeleteTree of this path fail.
	fail string
	// down makes every call fail.
	down bool
}

func (v *fakeVault) DeleteTree(_ context.Context, path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down || path == v.fail {
		return errors.New("HTTP 503: the vault is sealed")
	}
	v.deleted = append(v.deleted, path)
	for p := range v.paths {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(v.paths, p)
		}
	}
	return nil
}

func (v *fakeVault) ListChildren(_ context.Context, path string) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return nil, errors.New("HTTP 503: the vault is sealed")
	}
	seen := map[string]bool{}
	for p := range v.paths {
		rest, ok := strings.CutPrefix(p, path+"/")
		if !ok {
			continue
		}
		child, _, deeper := strings.Cut(rest, "/")
		if deeper {
			child += "/"
		}
		seen[child] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// fakeGroups is the identity provider's groups, each with its members.
type fakeGroups struct {
	mu      sync.Mutex
	members map[string][]string
	// fail makes every call fail; failDelete only the deletion.
	fail, failDelete bool
	deletes          int
	// realms are the realms asked about, in order.
	realms []string
	// scopes are the realm's client scopes; scopesDeleted the ones asked to
	// be removed.
	scopes        map[string]bool
	scopesDeleted []string
}

func (g *fakeGroups) DeleteGroup(_ context.Context, realm string, name string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.realms = append(g.realms, realm)
	if g.fail || g.failDelete {
		return false, errors.New("keycloak GET /admin/realms/demo/groups: 503 Service Unavailable")
	}
	g.deletes++
	_, existed := g.members[name]
	delete(g.members, name)
	return existed, nil
}

func (g *fakeGroups) DeleteClientScope(_ context.Context, realm string, name string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.realms = append(g.realms, realm)
	if g.fail || g.failDelete {
		return false, errors.New("keycloak GET /admin/realms/demo/client-scopes: 503 Service Unavailable")
	}
	existed := g.scopes[name]
	delete(g.scopes, name)
	g.scopesDeleted = append(g.scopesDeleted, name)
	return existed, nil
}

func (g *fakeGroups) GroupNames(_ context.Context, realm string, prefix string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.realms = append(g.realms, realm)
	if g.fail {
		return nil, errors.New("keycloak GET /admin/realms/demo/groups: 503 Service Unavailable")
	}
	var out []string
	for name := range g.members {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// purgeWorld is a cluster holding what an uninstalled app left behind, with
// every place a purge touches replaced by something that records it.
type purgeWorld struct {
	svc    *Service
	kube   *k8sfake.Clientset
	objs   client.Client
	vault  *fakeVault
	groups *fakeGroups

	mu sync.Mutex
	// jobs are the names of the Jobs created, in order.
	jobs []string
	// failJob makes the Job of this name end Failed; hangJob makes it never
	// end.
	failJob, hangJob string
}

// fastPurge shrinks the purge's waits to what a test can afford.
func fastPurge(t *testing.T) {
	t.Helper()
	budget, job, pvc, record, poll, gone := purgeBudget, purgeJobWait, pvcDeletionTimeout, purgeRecordWait, purgePoll, purgeWait
	purgeBudget, purgeJobWait, pvcDeletionTimeout, purgeRecordWait = 5*time.Second, time.Second, time.Second, time.Second
	purgePoll, purgeWait = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		purgeBudget, purgeJobWait, pvcDeletionTimeout, purgeRecordWait, purgePoll, purgeWait = budget, job, pvc, record, poll, gone
	})
}

// wikiProfile declares every kind of store: a PostgreSQL database, a bucket,
// a cache and an extension with secrets of its own.
func wikiProfile() *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{Chart: &gentianov1alpha1.ChartRef{
				Repository: "oci://example.invalid/charts", Name: "wikichart", Version: "1.0.0"}},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL},
				Storage:  &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}},
				Cache:    &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis},
			}},
			Extensions: []gentianov1alpha1.AppSidecarSpec{{Name: "mcp"}},
		},
	}
}

// provisionedRecord is the tenant's record of the stores provisioning made.
func provisionedRecord(t *testing.T, tenant string, entries map[string]backup.Provisioned) *corev1.ConfigMap {
	t.Helper()
	record := backup.NewProvisionedRecord(tenant)
	for app, p := range entries {
		if _, err := backup.RecordProvisioned(record, app, p); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

// provisionedFor reads one app's entry back.
func (w *purgeWorld) provisionedFor(t *testing.T, app string) backup.Provisioned {
	t.Helper()
	all, err := w.svc.provisioned(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	return all[app]
}

func databaseRecord(tenant, app string) *unstructured.Unstructured {
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(cnpgDatabaseGVK)
	db.SetName(cnpgDatabaseName(tenant, app))
	db.SetNamespace(layout.System("postgresql"))
	db.SetLabels(map[string]string{meta.TenantLabel: tenant, "gentianos.io/app": app})
	return db
}

func postgresPod(name, role string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: layout.System("postgresql"),
			Labels: map[string]string{"cnpg.io/cluster": "postgres", "cnpg.io/instanceRole": role}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// keptClaim is a volume claim an uninstall left in place: Helm's own, still
// naming the release that made it.
func keptClaim(name, release string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "tenant-demo",
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		Annotations: map[string]string{"meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": "tenant-demo", "helm.sh/resource-policy": "keep"},
	}}
}

func helmRecord(release string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + release + ".v1", Namespace: "tenant-demo",
			Labels: map[string]string{"owner": "helm", "name": release}},
		Type: helmReleaseSecretType,
	}
}

// newPurgeWorld is tenant demo with an uninstalled wiki that left everything
// behind, beside an installed drive whose data must survive whatever is done
// to wiki. objects are added to the API server's; kube objects to the
// clientset's.
func newPurgeWorld(t *testing.T, tenant *gentianov1alpha1.Tenant, objects []client.Object, kube ...runtime.Object) *purgeWorld {
	t.Helper()
	fastPurge(t)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if tenant == nil {
		tenant = demoTenant()
		tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "drive"}}
	}
	objects = append(objects, tenant, databaseRecord("demo", "wiki"), databaseRecord("demo", "drive"))
	// What provisioning wrote down when the two apps were installed, unless
	// the test brings a record of its own.
	// wiki's entry is what its profile declared, as provisioning records it.
	hasRecord, wiki := false, wikiProfile()
	for _, o := range objects {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == backup.ProvisionedRecordKey("demo").Name {
			hasRecord = true
		}
		if cp, ok := o.(*gentianov1alpha1.ComponentProfile); ok && cp.Name == "wiki" {
			wiki = cp
		}
	}
	if !hasRecord {
		objects = append(objects, provisionedRecord(t, "demo", map[string]backup.Provisioned{
			"wiki": backup.ProvisionedOf(backup.InventoryOf(tenant, "wiki", wiki)),
			"drive": {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL,
				Database: "demo_drive", DatabaseUser: "demo_drive"},
		}))
	}
	kube = append(kube,
		// The replica sorts first: whichever pod the API server lists first
		// is the wrong one to ask.
		postgresPod("postgres-1", "replica"), postgresPod("postgres-2", "primary"),
		keptClaim("wiki-release-data", "wiki-release"),
		keptClaim("wiki-mcp-release-cache", "wiki-mcp-release"),
		keptClaim("drive-release-data", "drive-release"),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "wiki-install-done", Namespace: "tenant-demo"},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "wiki-release-data"}}}}},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wiki-llm", Namespace: "tenant-demo", Labels: map[string]string{
			meta.TenantLabel: "demo", "gentianos.io/app": "wiki", meta.ManagedByLabel: meta.ManagedByValue}}},
		helmRecord("drive-release"),
	)
	w := &purgeWorld{
		kube: k8sfake.NewClientset(kube...),
		objs: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		vault: &fakeVault{paths: map[string]bool{
			"gentian-os/tenants/demo/apps/wiki/database":         true,
			"gentian-os/tenants/demo/apps/wiki/internal/key":     true,
			"gentian-os/tenants/demo/apps/wiki-mcp/internal/key": true,
			"gentian-os/tenants/demo/apps/drive/database":        true,
			"gentian-os/tenants/demo/admin":                      true,
		}},
		groups: &fakeGroups{members: map[string][]string{
			keycloak.TenantAppGroup("demo", "wiki"): {"ada", "bob"},
			// An extension that signs people in has a group of its own.
			keycloak.TenantAppGroup("demo", "wiki-mcp"): {"ada"},
			keycloak.TenantAppGroup("demo", "drive"):    {"ada"},
			"gentian:tenant:demo:admins":                {"ada"},
		}},
	}
	w.kube.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.jobs = append(w.jobs, job.Name)
		switch job.Name {
		case w.hangJob:
		case w.failJob:
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}
		default:
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return false, nil, nil
	})
	w.svc = &Service{client: w.objs, clientset: w.kube, vault: w.vault, groups: w.groups}
	return w
}

// created reports whether a Job of this name was run.
func (w *purgeWorld) created(job string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, name := range w.jobs {
		if name == job {
			return true
		}
	}
	return false
}

// untouched reports whether nothing at all was destroyed.
func (w *purgeWorld) untouched(t *testing.T) bool {
	t.Helper()
	return len(w.jobs)+len(w.vault.deleted)+w.groups.deletes == 0 &&
		w.claimExists(t, "wiki-release-data") && w.recordExists(t, "wiki")
}

func (w *purgeWorld) claimExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := w.kube.CoreV1().PersistentVolumeClaims("tenant-demo").Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func (w *purgeWorld) recordExists(t *testing.T, app string) bool {
	t.Helper()
	db := databaseRecord("demo", app)
	err := w.objs.Get(context.Background(), client.ObjectKeyFromObject(db), db)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// everyKind is the order a purge works in: the inventory's teardown order.
var everyKind = []string{KindFiles, KindCache, KindObjectStorage, KindDatabase, KindAccessGroup, KindCredentials, KindRecords}

// A purge destroys every kind of data the app owns and nothing of anybody
// else's: the database and its role on the primary, the bucket and the cache
// user by a Job each, the files, every vault path of the app and of its
// extension, the access group with its memberships, and the records
// provisioning left.
func TestAPurgeDestroysEveryKindTheAppOwnsAndNothingElse(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})

	res, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "purged" || !res.Purged || res.Complete == nil || !*res.Complete {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(res.Destroyed, everyKind) {
		t.Fatalf("destroyed = %v, want %v", res.Destroyed, everyKind)
	}

	// The stores, each by its Job, in teardown order: the cache user, the
	// bucket, then the database -- and the database's record after it.
	if want := []string{"redis-acl-delete-demo-wiki", "s3-delete-demo-wiki", "pg-delete-demo-wiki"}; !reflect.DeepEqual(w.jobs, want) {
		t.Errorf("deletion Jobs = %v, want %v", w.jobs, want)
	}
	if w.recordExists(t, "wiki") || !w.recordExists(t, "drive") {
		t.Error("the database records: wiki's must go and drive's must stay")
	}
	// And the record of what was provisioned: wiki's entry is gone with its
	// stores, drive's is as it was.
	if left := w.provisionedFor(t, "wiki"); !left.Empty() {
		t.Errorf("the purged app is still on record as holding %+v", left)
	}
	if !w.provisionedFor(t, "drive").Has(backup.KindDatabase) {
		t.Error("another app's entry was taken out of the record")
	}

	// The files, with the finished pod that held one of them.
	if w.claimExists(t, "wiki-release-data") || w.claimExists(t, "wiki-mcp-release-cache") {
		t.Error("a volume claim of the app survived its purge")
	}
	if !w.claimExists(t, "drive-release-data") {
		t.Error("another app's volume claim was deleted")
	}
	if _, err := w.kube.CoreV1().Pods("tenant-demo").Get(context.Background(), "wiki-install-done", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the pod holding a purged claim is still there: %v", err)
	}

	// Every vault path the app owns: its own and its extension's.
	if want := []string{"gentian-os/tenants/demo/apps/wiki", "gentian-os/tenants/demo/apps/wiki-mcp"}; !reflect.DeepEqual(w.vault.deleted, want) {
		t.Errorf("vault paths deleted = %v, want %v", w.vault.deleted, want)
	}
	for _, kept := range []string{"gentian-os/tenants/demo/apps/drive/database", "gentian-os/tenants/demo/admin"} {
		if !w.vault.paths[kept] {
			t.Errorf("the vault path %s was deleted", kept)
		}
	}

	// The access group, and with it who was in it.
	for _, key := range []string{"wiki", "wiki-mcp"} {
		if _, there := w.groups.members[keycloak.TenantAppGroup("demo", key)]; there {
			t.Errorf("the access group of %s and its memberships outlived the purge", key)
		}
	}
	if len(w.groups.members[keycloak.TenantAppGroup("demo", "drive")]) != 1 || len(w.groups.members["gentian:tenant:demo:admins"]) != 1 {
		t.Errorf("another group was touched: %v", w.groups.members)
	}

	// The records.
	if _, err := w.kube.CoreV1().Secrets("tenant-demo").Get(context.Background(), "wiki-llm", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the app's labelled Secret is still there: %v", err)
	}
}

// Each step's failure is the purge's failure. It names the step, says what
// was already destroyed and what was not attempted, tells the caller to
// retry, and stops there: nothing later is destroyed on the strength of a
// state nobody has looked at.
func TestAFailingStepFailsThePurgeAndNamesItself(t *testing.T) {
	cases := []struct {
		step    string
		breakIt func(w *purgeWorld)
		says    string
	}{
		{KindFiles, func(w *purgeWorld) {
			w.kube.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("the storage backend refused")
			})
		}, "the storage backend refused"},
		{KindCache, func(w *purgeWorld) { w.failJob = "redis-acl-delete-demo-wiki" }, "redis-acl-delete-demo-wiki failed"},
		{KindObjectStorage, func(w *purgeWorld) { w.failJob = "s3-delete-demo-wiki" }, "s3-delete-demo-wiki failed"},
		{KindDatabase, func(w *purgeWorld) { w.failJob = "pg-delete-demo-wiki" }, "pg-delete-demo-wiki failed"},
		{KindAccessGroup, func(w *purgeWorld) { w.groups.failDelete = true }, "503"},
		{KindCredentials, func(w *purgeWorld) { w.vault.fail = "gentian-os/tenants/demo/apps/wiki-mcp" }, "the vault is sealed"},
		{KindRecords, func(w *purgeWorld) {
			w.kube.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.(k8stesting.ListAction).GetListRestrictions().Labels.String() == "owner=helm" {
					return false, nil, nil
				}
				return true, nil, errors.New("the API server is overloaded")
			})
		}, "the API server is overloaded"},
	}
	for i, c := range cases {
		t.Run(c.step, func(t *testing.T) {
			w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
			c.breakIt(w)

			res, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom@example.com")
			if res != nil {
				t.Fatalf("a purge that failed answered %+v", res)
			}
			var failed *PurgeError
			if !errors.As(err, &failed) {
				t.Fatalf("err = %v, want a PurgeError", err)
			}
			if failed.Step != c.step {
				t.Fatalf("failed step = %q, want %q (%v)", failed.Step, c.step, err)
			}
			if want := everyKind[:i]; strings.Join(failed.Destroyed, ",") != strings.Join(want, ",") {
				t.Errorf("already destroyed = %v, want %v", failed.Destroyed, want)
			}
			if want := everyKind[i+1:]; strings.Join(failed.Pending, ",") != strings.Join(want, ",") {
				t.Errorf("not attempted = %v, want %v", failed.Pending, want)
			}
			msg := err.Error()
			for _, want := range []string{c.says, kindLabels[c.step], "Retry the purge"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the message does not say %q: %s", want, msg)
				}
			}
			for _, done := range everyKind[:i] {
				if !strings.Contains(msg, kindLabels[done]) {
					t.Errorf("the message does not name %q as already destroyed: %s", kindLabels[done], msg)
				}
			}

			// Nothing after the failed step ran.
			after := map[string]bool{}
			for _, k := range everyKind[i+1:] {
				after[k] = true
			}
			if after[KindCredentials] && len(w.vault.deleted) != 0 {
				t.Errorf("credentials were deleted after %s failed: %v", c.step, w.vault.deleted)
			}
			if after[KindAccessGroup] && w.groups.deletes != 0 {
				t.Errorf("the access group was deleted after %s failed", c.step)
			}
			if after[KindDatabase] && (w.created("pg-delete-demo-wiki") || !w.recordExists(t, "wiki")) {
				t.Errorf("the database was dropped after %s failed", c.step)
			}
			// And the app is still known to hold data, so it can be found
			// and purged again -- asked once the identity provider answers
			// again, where that was what failed.
			w.groups.failDelete = false
			retained, err := w.svc.RetainedApps(context.Background(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			if c.step != KindRecords && (len(retained.Apps) != 1 || retained.Apps[0].Profile != "wiki") {
				t.Errorf("after a purge that stopped at %s the app is no longer listed as holding data: %+v", c.step, retained.Apps)
			}
		})
	}
}

// Every step can be repeated: a purge that stopped is finished by asking
// again, and a purge of an app with nothing left succeeds without finding
// anything to destroy.
func TestARetriedPurgeFinishesWhatTheFirstLeft(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.vault.fail = "gentian-os/tenants/demo/apps/wiki"

	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err == nil {
		t.Fatal("the first purge did not fail")
	}
	if !w.vault.paths["gentian-os/tenants/demo/apps/wiki/database"] {
		t.Fatal("the fixture did not keep the credentials the failed step was to delete")
	}

	w.vault.fail = ""
	for attempt := 2; attempt <= 3; attempt++ {
		res, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if !reflect.DeepEqual(res.Destroyed, everyKind) || !*res.Complete {
			t.Fatalf("attempt %d: %+v", attempt, res)
		}
	}
	for path := range w.vault.paths {
		if strings.Contains(path, "/apps/wiki") {
			t.Errorf("the vault path %s survived the retried purge", path)
		}
	}
	if !w.recordExists(t, "drive") || !w.claimExists(t, "drive-release-data") {
		t.Error("repeating the purge reached another app's data")
	}
}

// Whatever can be known beforehand about whether a purge can finish is
// established before anything is destroyed: that the database server has a
// primary to drop on, that the vault and the identity provider answer, and
// that the engine is one this platform can drop. A purge that would have
// stopped half-way for one of these is refused instead, with everything in
// place.
func TestAPurgeThatCannotFinishDestroysNothing(t *testing.T) {
	odd := wikiProfile()
	odd.Spec.Requires.Services.Database.Engine = "oracle"
	cases := []struct {
		name    string
		profile *gentianov1alpha1.ComponentProfile
		breakIt func(t *testing.T, w *purgeWorld)
		says    string
	}{
		{"no PostgreSQL primary", wikiProfile(), func(t *testing.T, w *purgeWorld) {
			if err := w.kube.CoreV1().Pods(layout.System("postgresql")).Delete(context.Background(), "postgres-2", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}, "no running PostgreSQL primary"},
		{"no vault configured", wikiProfile(), func(_ *testing.T, w *purgeWorld) { w.svc.vault = nil }, "no connection to the vault"},
		{"the vault does not answer", wikiProfile(), func(_ *testing.T, w *purgeWorld) { w.vault.down = true }, "the vault does not answer"},
		{"the identity provider does not answer", wikiProfile(), func(_ *testing.T, w *purgeWorld) { w.groups.fail = true }, "realm demo"},
		{"an engine nothing here can drop", odd, func(*testing.T, *purgeWorld) {}, `cannot drop a "oracle" database`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newPurgeWorld(t, nil, []client.Object{c.profile})
			c.breakIt(t, w)
			_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
			if !errors.Is(err, ErrCannotPurgeNow) {
				t.Fatalf("err = %v, want a refusal before anything is destroyed", err)
			}
			if !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "Nothing was destroyed") {
				t.Errorf("message = %s", err)
			}
			if !w.untouched(t) {
				t.Errorf("a purge that could not finish destroyed something: jobs=%v vault=%v", w.jobs, w.vault.deleted)
			}
		})
	}

	// Over HTTP: not the caller's mistake and not a half-done purge.
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.vault.down = true
	if code, body := purgeOverHTTP(t, w, "wiki"); code != http.StatusServiceUnavailable || !strings.Contains(body, "Nothing was destroyed") {
		t.Errorf("answered %d: %s", code, body)
	}
}

// The app's group is in the tenant's realm, which a tenant may name
// (spec.isolation.keycloakRealm) and the platform tenant does: it is not
// always called what the tenant is. The group's own name carries the
// tenant's either way.
func TestTheAccessGroupIsRemovedFromTheTenantsOwnRealm(t *testing.T) {
	tenant := demoTenant()
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "gentian"}
	w := newPurgeWorld(t, tenant, []client.Object{wikiProfile()})

	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
	if len(w.groups.realms) == 0 {
		t.Fatal("the identity provider was never asked")
	}
	for _, realm := range w.groups.realms {
		if realm != "gentian" {
			t.Fatalf("the identity provider was asked about realm %q; the tenant's realm is gentian (asked: %v)", realm, w.groups.realms)
		}
	}
	if _, there := w.groups.members[keycloak.TenantAppGroup("demo", "wiki")]; there {
		t.Error("the group was not removed")
	}

	// The read of what is retained asks the same realm.
	w.groups.realms = nil
	if _, err := w.svc.RetainedApps(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if len(w.groups.realms) != 1 || w.groups.realms[0] != "gentian" {
		t.Errorf("the retained read asked %v", w.groups.realms)
	}
}

// A purge is refused, and destroys nothing, while the app is the tenant's:
// installed, switched on as an add-on, or still being taken down -- which
// lasts until Helm has finished uninstalling its release, not merely until
// its Component is gone. A name that is not an app's is refused outright.
func TestAPurgeIsRefusedWhileAnythingOfTheAppIsStillThere(t *testing.T) {
	installed := demoTenant()
	installed.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "wiki"}, {Profile: "cloud", Addons: []string{"cloud-calendar"}}}

	cases := []struct {
		name    string
		tenant  *gentianov1alpha1.Tenant
		kube    []runtime.Object
		profile string
		want    error
	}{
		{"installed", installed, nil, "wiki", ErrStillInstalled},
		{"an add-on that is switched on", installed, nil, "cloud-calendar", ErrStillInstalled},
		{"its release is still being uninstalled", nil, []runtime.Object{helmRecord("wiki-release")}, "wiki", ErrStillRemoving},
		{"its extension's release is still being uninstalled", nil, []runtime.Object{helmRecord("wiki-mcp-release")}, "wiki", ErrStillRemoving},
		{"a directly delivered chart is still being uninstalled", nil, []runtime.Object{helmRecord("tenant-demo-wiki")}, "wiki", ErrStillRemoving},
		{"the desktop's own store", nil, nil, "shell", ErrNotAnApp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newPurgeWorld(t, c.tenant, []client.Object{wikiProfile()}, c.kube...)
			_, err := w.svc.PurgeApp(context.Background(), "demo", c.profile, "tom")
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if !w.untouched(t) {
				t.Errorf("a refused purge destroyed something: jobs=%v vault=%v", w.jobs, w.vault.deleted)
			}
		})
	}

	// The same over HTTP: a conflict, which the caller may retry.
	w := newPurgeWorld(t, installed, []client.Object{wikiProfile()})
	if code, _ := purgeOverHTTP(t, w, "wiki"); code != http.StatusConflict {
		t.Errorf("purging an installed app answered %d, want 409", code)
	}
}

// Another app's release that merely begins with this app's name is not this
// app being removed, when the profile says what the app's extensions are.
func TestAnotherAppsReleaseDoesNotHoldAPurgeBack(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()}, helmRecord("wiki-pro-release"))
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
}

// Which stores an app has is declared by its profile and by nothing else.
// When the profile is not on the cluster a purge is refused and destroys
// nothing: it used to assume a PostgreSQL database, look at nothing else and
// answer success. The refusal says what the administrator can do, and the
// app is still listed as holding data.
func TestAPurgeWithoutTheProfileIsRefusedAndDestroysNothing(t *testing.T) {
	w := newPurgeWorld(t, nil, nil)

	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	if !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v", err)
	}
	if !w.untouched(t) {
		t.Errorf("a purge without the profile destroyed something: jobs=%v vault=%v", w.jobs, w.vault.deleted)
	}

	code, body := purgeOverHTTP(t, w, "wiki")
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
	for _, want := range []string{"ComponentProfile wiki is not on this cluster", "Nothing was destroyed", "catalogue source"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q: %s", want, body)
		}
	}
	for _, gone := range []string{"partially", "notExamined"} {
		if strings.Contains(body, gone) {
			t.Errorf("the answer still speaks of a partial purge (%q): %s", gone, body)
		}
	}

	retained, err := w.svc.RetainedApps(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	apps := retainedByProfile(retained.Apps)
	if wiki, ok := apps["wiki"]; !ok || wiki.ProfileAvailable || wiki.Kinds[KindDatabase] != RetainedPresent {
		t.Errorf("the app whose purge was refused is not listed as holding data without a profile: %+v", retained.Apps)
	}
}

// What an extension holds is the app's to destroy, unless what the extension's
// key spells is an app the tenant has: then the group, the vault path and the
// volumes under that name are in use, and are nobody else's to remove.
func TestWhatAnInstalledAppHoldsIsNotDestroyedAsAnExtensions(t *testing.T) {
	tenant := demoTenant()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "drive"}, {Profile: "wiki-mcp"}}
	w := newPurgeWorld(t, tenant, []client.Object{wikiProfile()})
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
	if _, there := w.groups.members[keycloak.TenantAppGroup("demo", "wiki-mcp")]; !there {
		t.Error("the group of an installed app was deleted because its name spells another app's extension")
	}
	if !w.vault.paths["gentian-os/tenants/demo/apps/wiki-mcp/internal/key"] {
		t.Error("the credentials of an installed app were deleted because its name spells another app's extension")
	}
	if !w.claimExists(t, "wiki-mcp-release-cache") {
		t.Error("the volume of an installed app was deleted because its name begins with the purged app's")
	}
	if _, there := w.groups.members[keycloak.TenantAppGroup("demo", "wiki")]; there {
		t.Error("the purged app's own group is still there")
	}
	if w.claimExists(t, "wiki-release-data") {
		t.Error("the purged app's own volume is still there")
	}
}

// A purge that did not complete is not a 2xx, and the caller is given the
// whole account: the step, what is already gone, and to retry.
func TestAnIncompletePurgeIsAnErrorTheCallerCanActOn(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.failJob = "s3-delete-demo-wiki"

	code, body := purgeOverHTTP(t, w, "wiki")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", code, body)
	}
	for _, want := range []string{"did not complete", "object storage", "Already destroyed: files, cache", "Not attempted: database, access group", "Retry the purge"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not say %q: %s", want, body)
		}
	}
}

// A purge gives up by itself, inside the time it is given, and says where:
// its caller's deadline is longer, so it is never cut off part-way by the
// request that asked for it.
func TestAPurgeThatRunsOutOfTimeSaysWhereItStopped(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.hangJob = "s3-delete-demo-wiki"
	purgeBudget, purgeJobWait = 60*time.Millisecond, time.Minute

	started := time.Now()
	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the purge took %s with a budget of %s", took, purgeBudget)
	}
	var failed *PurgeError
	if !errors.As(err, &failed) || failed.Step != KindObjectStorage {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(failed.Destroyed, []string{KindFiles, KindCache}) || !strings.Contains(err.Error(), "ran out of the") {
		t.Errorf("err = %v", err)
	}
}

// The operator gives a purge less time than its caller waits for it, so the
// answer -- finished, or stopped and saying where -- always arrives.
func TestAPurgeAnswersBeforeItsCallerStopsWaiting(t *testing.T) {
	if purgeWait >= purgeBudget {
		t.Fatalf("the wait for the app to be gone (%s) uses up the whole purge (%s)", purgeWait, purgeBudget)
	}
	if margin := lifecycle.PurgeDeadline - purgeBudget; margin < 20*time.Second {
		t.Fatalf("the director waits %s for a purge the operator may take %s over: %s is not enough to carry the answer back",
			lifecycle.PurgeDeadline, purgeBudget, margin)
	}
	for name, wait := range map[string]time.Duration{"a deletion Job": purgeJobWait, "the volume claims": pvcDeletionTimeout,
		"a Job's own deadline": time.Duration(purgeJobDeadlineSeconds) * time.Second} {
		if wait >= purgeBudget {
			t.Errorf("the wait for %s (%s) is not inside the purge's budget (%s)", name, wait, purgeBudget)
		}
	}
	if time.Duration(purgeJobDeadlineSeconds)*time.Second >= purgeJobWait {
		t.Error("a deletion Job's own deadline is not shorter than the wait for it: a hung Job would be reported as a timeout, not as a failure")
	}
}

// A second purge of the same app is turned away while the first runs, and
// does not queue behind it.
func TestASecondPurgeOfTheSameAppDoesNotQueue(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	unlock := w.svc.lockApp("demo", "wiki")
	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
}

func purgeOverHTTP(t *testing.T, w *purgeWorld, profile string) (int, string) {
	t.Helper()
	_, auth := cluster()
	h := &HTTPServer{Auth: auth, Service: w.svc}
	r := httptest.NewRequest("POST", "/v1/tenants/demo/actions/purge-app", strings.NewReader(`{"profile":"`+profile+`"}`))
	r.Header.Set("Authorization", "Bearer director")
	rec := httptest.NewRecorder()
	h.routes().ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// A store on record is destroyed whether or not the profile still declares
// it: a profile that has since dropped its bucket has not removed the bucket.
// And a store whose destruction failed stays on record.
func TestAPurgeDestroysWhatIsOnRecordAndForgetsOnlyWhatItDestroyed(t *testing.T) {
	lean := wikiProfile()
	lean.Spec.Requires.Services.Storage = nil
	lean.Spec.Requires.Services.Cache = nil
	// Provisioned when the profile still declared all three.
	w := newPurgeWorld(t, nil, []client.Object{lean, provisionedRecord(t, "demo", map[string]backup.Provisioned{
		"wiki": backup.ProvisionedOf(backup.InventoryOf(demoTenant(), "wiki", wikiProfile())),
	})})
	w.failJob = "s3-delete-demo-wiki"

	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	var perr *PurgeError
	if !errors.As(err, &perr) || perr.Step != KindObjectStorage {
		t.Fatalf("err = %v, want the purge to stop at the bucket the record names", err)
	}
	left := w.provisionedFor(t, "wiki")
	if left.Has(backup.KindCache) {
		t.Error("the cache user was destroyed and is still on record")
	}
	if !left.Has(backup.KindObjectStorage) || !left.Has(backup.KindDatabase) {
		t.Errorf("stores that were not destroyed are off the record: %+v", left)
	}

	w.failJob = ""
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
	if !w.created("pg-delete-demo-wiki") || !w.provisionedFor(t, "wiki").Empty() {
		t.Errorf("the retried purge left %+v on record; Jobs %v", w.provisionedFor(t, "wiki"), w.jobs)
	}
}

// A record that cannot be read refuses the purge with nothing destroyed.
func TestAPurgeIsRefusedWhenTheRecordCannotBeRead(t *testing.T) {
	broken := backup.NewProvisionedRecord("demo")
	broken.Data = map[string]string{"wiki": "{not json"}
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile(), broken})
	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	if !errors.Is(err, ErrCannotPurgeNow) || !w.untouched(t) {
		t.Fatalf("err = %v, untouched = %v", err, w.untouched(t))
	}
}

// fakeModels is the model gateway as a purge uses it.
type fakeModels struct {
	keys    map[string]bool
	down    bool
	deleted []string
}

func (m *fakeModels) KeyExists(_ context.Context, alias string) (bool, error) {
	if m.down {
		return false, errors.New("model gateway /key/list answered 502")
	}
	return m.keys[alias], nil
}

func (m *fakeModels) DeleteKey(_ context.Context, alias string) (bool, error) {
	if m.down {
		return false, errors.New("model gateway /key/list answered 502")
	}
	existed := m.keys[alias]
	delete(m.keys, alias)
	m.deleted = append(m.deleted, alias)
	return existed, nil
}

// The key an app called models with is removed at its purge, by the alias
// on record, and taken off the record. A gateway that does not answer
// refuses the purge before anything is destroyed; a cluster with no gateway
// has no key left to remove.
func TestAPurgeRemovesTheAppsModelKey(t *testing.T) {
	withKey := func(t *testing.T) *purgeWorld {
		entry := backup.ProvisionedOf(backup.InventoryOf(demoTenant(), "wiki", wikiProfile()))
		entry.ModelKey = "demo-wiki"
		return newPurgeWorld(t, nil, []client.Object{wikiProfile(), provisionedRecord(t, "demo", map[string]backup.Provisioned{
			"wiki":  entry,
			"drive": {ModelKey: "demo-drive"},
		})})
	}

	w := withKey(t)
	models := &fakeModels{keys: map[string]bool{"demo-wiki": true, "demo-drive": true}}
	w.svc.models = func(context.Context) (ModelKeys, bool, error) { return models, true, nil }
	res, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{KindFiles, KindModelAccess, KindCache, KindObjectStorage, KindDatabase, KindAccessGroup, KindCredentials, KindRecords}
	if !reflect.DeepEqual(res.Destroyed, want) {
		t.Errorf("destroyed = %v, want %v", res.Destroyed, want)
	}
	if models.keys["demo-wiki"] || !models.keys["demo-drive"] {
		t.Errorf("keys = %v, want wiki's removed and drive's kept", models.keys)
	}
	if !w.provisionedFor(t, "wiki").Empty() || w.provisionedFor(t, "drive").ModelKey != "demo-drive" {
		t.Errorf("record: wiki %+v, drive %+v", w.provisionedFor(t, "wiki"), w.provisionedFor(t, "drive"))
	}

	// The gateway does not answer: refused, nothing destroyed.
	w = withKey(t)
	down := &fakeModels{down: true}
	w.svc.models = func(context.Context) (ModelKeys, bool, error) { return down, true, nil }
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); !errors.Is(err, ErrCannotPurgeNow) || !w.untouched(t) {
		t.Fatalf("err = %v, untouched = %v", err, w.untouched(t))
	}

	// No gateway on the cluster any more: the purge completes, and the key
	// is off the record.
	w = withKey(t)
	w.svc.models = func(context.Context) (ModelKeys, bool, error) { return nil, false, nil }
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
	if !w.provisionedFor(t, "wiki").Empty() {
		t.Errorf("record = %+v", w.provisionedFor(t, "wiki"))
	}
}

// The client scope an app's sign-in pack describes is the realm's, not the
// client's: it outlived the client at every uninstall and nothing removed
// it. A purge does -- unless an app the tenant still has names the same
// scope, or the name is one of the identity provider's own.
func TestAPurgeRemovesTheAppsSignInScope(t *testing.T) {
	oidcProfile := func(name, pack string) *gentianov1alpha1.ComponentProfile {
		p := wikiProfile()
		p.Name = name
		p.Spec.Extensions = nil
		p.Spec.Requires.Services.Identity = &gentianov1alpha1.IdentityRequirement{
			OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: name, OIDCPackRef: pack}}
		return p
	}
	packs := &gentianov1alpha1.OIDCPackCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue"},
		Spec: gentianov1alpha1.OIDCPackCatalogSpec{Packs: map[string]gentianov1alpha1.OIDCPackSpec{
			"wiki-pack":    {ScopeName: "wiki-scope", ClientRole: "user", EntitlementGroup: "wiki"},
			"shared-pack":  {ScopeName: "shared-scope", ClientRole: "user", EntitlementGroup: "shared"},
			"builtin-pack": {ScopeName: "profile", ClientRole: "user", EntitlementGroup: "x"},
		}},
	}
	cases := []struct {
		name, pack string
		installed  *gentianov1alpha1.ComponentProfile
		deleted    []string
	}{
		{"its own scope", "wiki-pack", nil, []string{"wiki-scope"}},
		{"a scope an installed app names too", "shared-pack", oidcProfile("drive", "shared-pack"), nil},
		{"one of the identity provider's own", "builtin-pack", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects := []client.Object{oidcProfile("wiki", c.pack), packs}
			if c.installed != nil {
				objects = append(objects, c.installed)
			}
			w := newPurgeWorld(t, nil, objects)
			w.groups.scopes = map[string]bool{"wiki-scope": true, "shared-scope": true, "profile": true}
			res, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(res.Destroyed, KindSignInScope) {
				t.Errorf("destroyed = %v, want the sign-in scope among them", res.Destroyed)
			}
			if !reflect.DeepEqual(w.groups.scopesDeleted, c.deleted) {
				t.Errorf("scopes deleted = %v, want %v", w.groups.scopesDeleted, c.deleted)
			}
			if !w.groups.scopes["profile"] {
				t.Error("the identity provider's own profile scope was deleted")
			}
		})
	}
}
