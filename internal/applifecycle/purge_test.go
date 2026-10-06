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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	fail    bool
	deletes int
}

func (g *fakeGroups) DeleteGroup(_ context.Context, _ string, name string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return false, errors.New("keycloak GET /admin/realms/demo/groups: 503 Service Unavailable")
	}
	g.deletes++
	_, existed := g.members[name]
	delete(g.members, name)
	return existed, nil
}

func (g *fakeGroups) GroupNames(_ context.Context, _ string, prefix string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
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
	// sql is every statement run, as "<pod>: <statement>".
	sql []string
	// failSQL makes a statement containing it answer with a server error.
	failSQL string
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
	w.svc = &Service{client: w.objs, clientset: w.kube, vault: w.vault, groups: w.groups,
		exec: func(_ context.Context, ns, pod, container string, command []string) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			stmt := command[len(command)-1]
			if ns != layout.System("postgresql") || container != "postgres" {
				return "", fmt.Errorf("exec into %s/%s[%s]", ns, pod, container)
			}
			w.sql = append(w.sql, pod+": "+stmt)
			if w.failSQL != "" && strings.Contains(stmt, w.failSQL) {
				return "ERROR:  cannot execute DROP DATABASE in a read-only transaction", nil
			}
			return "", nil
		}}
	return w
}

func (w *purgeWorld) ran(fragment string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.sql {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
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

var everyKind = []string{KindDatabase, KindObjectStorage, KindCache, KindFiles, KindCredentials, KindAccessGroup, KindRecords}

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
	if res.Status != "purged" || !res.Purged || res.Complete == nil || !*res.Complete || len(res.NotExamined) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(res.Destroyed, everyKind) {
		t.Fatalf("destroyed = %v, want %v", res.Destroyed, everyKind)
	}

	// The database, on the primary and nowhere else.
	for _, want := range []string{`DROP DATABASE IF EXISTS "demo_wiki"`, `DROP ROLE IF EXISTS "demo_wiki"`} {
		if !w.ran("postgres-2: " + want) {
			t.Errorf("the primary was not asked to %s; ran %v", want, w.sql)
		}
	}
	if w.ran("postgres-1: ") {
		t.Errorf("a replica was asked to drop something: %v", w.sql)
	}
	if w.ran("demo_drive") {
		t.Errorf("another app's database was touched: %v", w.sql)
	}
	if w.recordExists(t, "wiki") || !w.recordExists(t, "drive") {
		t.Error("the database records: wiki's must go and drive's must stay")
	}

	// The bucket and the cache user.
	if want := []string{"s3-delete-demo-wiki", "redis-acl-delete-demo-wiki"}; !reflect.DeepEqual(w.jobs, want) {
		t.Errorf("deletion Jobs = %v, want %v", w.jobs, want)
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
		{KindDatabase, func(w *purgeWorld) { w.failSQL = "DROP DATABASE" }, "read-only transaction"},
		{KindObjectStorage, func(w *purgeWorld) { w.failJob = "s3-delete-demo-wiki" }, "s3-delete-demo-wiki failed"},
		{KindCache, func(w *purgeWorld) { w.failJob = "redis-acl-delete-demo-wiki" }, "redis-acl-delete-demo-wiki failed"},
		{KindFiles, func(w *purgeWorld) {
			w.kube.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("the storage backend refused")
			})
		}, "the storage backend refused"},
		{KindCredentials, func(w *purgeWorld) { w.vault.fail = "gentian-os/tenants/demo/apps/wiki-mcp" }, "the vault is sealed"},
		{KindAccessGroup, func(w *purgeWorld) { w.groups.fail = true }, "503"},
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
			if after[KindFiles] && !w.claimExists(t, "wiki-release-data") {
				t.Errorf("files were deleted after %s failed", c.step)
			}
			// And the app is still known to hold data, so it can be found
			// and purged again -- asked once the identity provider answers
			// again, where that was what failed.
			w.groups.fail = false
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

// A replica refuses DROP DATABASE, so the statements go to the primary; and
// with no primary to ask the purge fails rather than skip the database.
func TestTheDatabaseIsDroppedOnThePrimaryOrNotAtAll(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	if err := w.kube.CoreV1().Pods(layout.System("postgresql")).Delete(context.Background(), "postgres-2", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	var failed *PurgeError
	if !errors.As(err, &failed) || failed.Step != KindDatabase {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "no running PostgreSQL primary") || !strings.Contains(err.Error(), "Nothing had been destroyed") {
		t.Errorf("message = %s", err)
	}
	if len(w.sql) != 0 {
		t.Errorf("statements were run with no primary: %v", w.sql)
	}
	if !w.recordExists(t, "wiki") {
		t.Error("the database's record was removed although the database was not dropped")
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
			if len(w.sql)+len(w.jobs)+len(w.vault.deleted)+w.groups.deletes != 0 || !w.claimExists(t, "wiki-release-data") {
				t.Errorf("a refused purge destroyed something: sql=%v jobs=%v vault=%v", w.sql, w.jobs, w.vault.deleted)
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

// When the app's ComponentProfile is no longer on the cluster nothing says
// which stores it had. The purge does what it did before -- a PostgreSQL
// database is assumed, the app's own paths, files, group and records go --
// and answers success for that alone: the answer is marked incomplete and
// names the kinds it did not examine. Whether such a purge should be refused
// instead is undecided; the status code is unchanged until it is.
func TestAPurgeWithoutTheProfileSaysWhatItDidNotExamine(t *testing.T) {
	w := newPurgeWorld(t, nil, nil)

	code, body := purgeOverHTTP(t, w, "wiki")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	var res Result
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "partially-purged" || res.Complete == nil || *res.Complete {
		t.Fatalf("the answer claims a complete purge: %s", body)
	}
	want := []string{KindObjectStorage, KindCache, notExaminedMariaDB, notExaminedExtensionCredentials, notExaminedExtensionGroups, notExaminedChartNamedFiles}
	if !reflect.DeepEqual(res.NotExamined, want) {
		t.Errorf("notExamined = %v, want %v", res.NotExamined, want)
	}
	if want := []string{KindDatabase, KindFiles, KindCredentials, KindAccessGroup, KindRecords}; !reflect.DeepEqual(res.Destroyed, want) {
		t.Errorf("destroyed = %v, want %v", res.Destroyed, want)
	}
	if !strings.Contains(res.Message, "no longer on this cluster") || !strings.Contains(res.Message, "object storage") {
		t.Errorf("message = %s", res.Message)
	}

	// Which stores it purges is what it was: PostgreSQL assumed, no bucket
	// or cache Job, and the extension's vault path left alone because
	// nothing declares the extension.
	if !w.ran(`DROP DATABASE IF EXISTS "demo_wiki"`) {
		t.Error("the assumed PostgreSQL database was not dropped")
	}
	if len(w.jobs) != 0 {
		t.Errorf("stores the profile would have declared were guessed at: %v", w.jobs)
	}
	if want := []string{"gentian-os/tenants/demo/apps/wiki"}; !reflect.DeepEqual(w.vault.deleted, want) {
		t.Errorf("vault paths deleted = %v, want %v", w.vault.deleted, want)
	}
	if _, there := w.groups.members[keycloak.TenantAppGroup("demo", "wiki-mcp")]; !there {
		t.Error("the extension's group was deleted although nothing declares the extension")
	}
	// What was not examined is still reported as held, under the name a
	// purge would have to be asked for to remove it.
	retained, err := w.svc.RetainedApps(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.Apps) != 1 || retained.Apps[0].Profile != "wiki-mcp" {
		t.Errorf("after the incomplete purge the cluster reports %+v, want what is left under wiki-mcp", retained.Apps)
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
	for _, want := range []string{"did not complete", "object storage", "Already destroyed: database", "Not attempted: cache, files", "Retry the purge"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not say %q: %s", want, body)
		}
	}
}

// With no vault to talk to, the credentials are not destroyed and the purge
// says so. It used to report them destroyed.
func TestAPurgeWithNoVaultFailsAtTheCredentials(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.svc.vault = nil

	_, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom")
	var failed *PurgeError
	if !errors.As(err, &failed) || failed.Step != KindCredentials {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "no credential was deleted") {
		t.Errorf("message = %s", err)
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
	if !reflect.DeepEqual(failed.Destroyed, []string{KindDatabase}) || !strings.Contains(err.Error(), "ran out of the") {
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

// The deletion scripts end in success only when what they were asked to
// remove is verifiably gone: no command's failure is discarded.
func TestTheDeletionScriptsDiscardNoFailure(t *testing.T) {
	for name, script := range map[string]string{
		"mariadb": mariadbDeleteScript,
		"minio":   minioPurgeScript("demo-wiki"),
		"redis":   redisPurgeScript("demo-wiki"),
	} {
		if !strings.HasPrefix(script, "set -eu\n") {
			t.Errorf("%s: the script does not stop at the first failing command", name)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") {
				t.Errorf("%s: a failure is discarded: %s", name, line)
			}
		}
	}
	minio := minioPurgeScript("demo-wiki")
	for _, want := range []string{`mc rb --force "gentian/demo-wiki"`, "mc admin user rm", "mc admin policy rm", `arn:aws:s3:::demo-wiki"`} {
		if !strings.Contains(minio, want) {
			t.Errorf("the object-storage script does not %s", want)
		}
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
