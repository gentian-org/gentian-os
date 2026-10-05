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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// memVault is the vault as the Seeder sees it: write-once records by path.
type memVault map[string]map[string]string

func (v memVault) PutOnce(_ context.Context, path string, data map[string]string) error {
	if _, ok := v[path]; !ok {
		v[path] = data
	}
	return nil
}

func (v memVault) Put(_ context.Context, path string, data map[string]string) error {
	v[path] = data
	return nil
}

func (v memVault) Get(_ context.Context, path string) (map[string]string, error) {
	return v[path], nil
}

var cnpgClusterGVK = schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: "Cluster"}

func componentDatabaseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		cnpgClusterGVK,
		{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind},
		externalSecretGVK,
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func desktopComponentFixture(tenant string) *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name = DesktopComponentName
	comp.Namespace = "tenant-" + tenant
	comp.UID = "desktop-uid"
	return comp
}

// A tenant's desktop database is made on the tenant postgres the way an
// app's is, under the names exports and restores already use for it: the
// vault record, then the role Job, then the Database resource, then the
// credential in the desktop's namespace. Until a step has finished the
// requirement waits on it and says which.
func TestATenantDesktopsDatabaseIsMadeOnTheTenantPostgres(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	vault := memVault{}
	r := &ComponentReconciler{Client: c, Scheme: s, KernelRealm: "kernel", Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}
	tenant := acmeTenantFixture()
	comp := desktopComponentFixture("acme")

	step := func() (bool, string) {
		t.Helper()
		ready, reason, _, err := r.ensureDatabaseRequirement(ctx, comp, tenant)
		if err != nil {
			t.Fatalf("ensureDatabaseRequirement: %v", err)
		}
		return ready, reason
	}

	// No tenant postgres: nothing is made, and the requirement says why.
	if ready, reason := step(); ready || reason != "DatabaseUnavailable" {
		t.Fatalf("without a tenant postgres: ready=%v reason=%q", ready, reason)
	}
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(cnpgClusterGVK)
	pg.SetName(cnpgClusterName)
	pg.SetNamespace(postgresNamespace)
	if err := c.Create(ctx, pg); err != nil {
		t.Fatal(err)
	}

	if ready, reason := step(); ready || reason != "DatabaseProvisioning" {
		t.Fatalf("first pass: ready=%v reason=%q", ready, reason)
	}
	record := vault[secrets.CategoryPath("acme", portalShellAppName, "database")]
	if record["user"] != roleUserName("acme", portalShellAppName) || record["name"] != databaseName(tenant, portalShellAppName) || record["password"] == "" {
		t.Fatalf("vault record = %v", record)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: roleJobName("acme", portalShellAppName), Namespace: postgresNamespace}, job); err != nil {
		t.Fatalf("role Job: %v", err)
	}

	// The Job has not finished: still waiting, and no Database yet.
	if ready, _ := step(); ready {
		t.Fatal("ready before the role Job finished")
	}
	// A Job an earlier build made, whose pod the namespace would refuse, is
	// not waited on: it is deleted and the current one made in its place.
	job.Spec.Template.Spec.Containers[0].SecurityContext = nil
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	step()
	step()
	if err := c.Get(ctx, types.NamespacedName{Name: roleJobName("acme", portalShellAppName), Namespace: postgresNamespace}, job); err != nil {
		t.Fatalf("role Job after replacement: %v", err)
	}
	if job.Spec.Template.Spec.Containers[0].SecurityContext == nil {
		t.Fatal("the outdated role Job was kept")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if ready, _ := step(); ready {
		t.Fatal("ready before the Database was applied")
	}
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind})
	if err := c.Get(ctx, types.NamespacedName{Name: databaseCRName("acme", portalShellAppName), Namespace: postgresNamespace}, db); err != nil {
		t.Fatalf("Database: %v", err)
	}
	if db.GetLabels()[tenantLabel] != "acme" {
		t.Fatalf("Database labels = %v; a purge finds the database by its tenant label", db.GetLabels())
	}
	_ = unstructured.SetNestedField(db.Object, true, "status", "applied")
	if err := c.Update(ctx, db); err != nil {
		t.Fatal(err)
	}

	// Applied: the credential is delivered from the vault record.
	if ready, _ := step(); ready {
		t.Fatal("ready before the credential arrived")
	}
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: comp.Name + componentDatabaseSecretSuffix, Namespace: comp.Namespace}, es); err != nil {
		t.Fatalf("ExternalSecret: %v", err)
	}
	refs, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, raw := range refs {
		key, _, _ := unstructured.NestedString(raw.(map[string]interface{}), "remoteRef", "key")
		if key != secrets.CategoryPath("acme", portalShellAppName, "database") {
			t.Fatalf("credential read from %q", key)
		}
	}
	if len(refs) != 5 {
		t.Fatalf("credential reads %d properties, want host, port, name, user and password", len(refs))
	}
	_ = unstructured.SetNestedSlice(es.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := c.Update(ctx, es); err != nil {
		t.Fatal(err)
	}
	if ready, reason := step(); !ready {
		t.Fatalf("not ready once the credential arrived: %q", reason)
	}
}

// Without a vault there is no password to give the role, so nothing is made.
func TestATenantDesktopsDatabaseWaitsWithoutAVault(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(cnpgClusterGVK)
	pg.SetName(cnpgClusterName)
	pg.SetNamespace(postgresNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pg).Build()
	r := &ComponentReconciler{Client: c, Scheme: s, KernelRealm: "kernel"}

	ready, reason, _, err := r.ensureDatabaseRequirement(ctx, desktopComponentFixture("acme"), acmeTenantFixture())
	if err != nil || ready || reason != "DatabaseUnavailable" {
		t.Fatalf("ready=%v reason=%q err=%v", ready, reason, err)
	}
}

// The system namespaces refuse a pod that may escalate privileges or runs
// without a seccomp profile, and a Job whose pod is refused has no pod: it
// stays Running and whatever waits on it waits for ever. Every provisioning
// client container is built to be admitted there.
func TestProvisioningContainersAreAdmittedInTheSystemNamespaces(t *testing.T) {
	for name, c := range map[string]corev1.Container{
		"psql":    psqlContainer("x", "true", ""),
		"mariadb": mariadbContainer("x", "true", "db", "user"),
		"redis":   redisContainer("x", "user", "prefix", "true"),
		"minio":   minioContainer("x", "bucket", "true"),
	} {
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: privilege escalation is not refused", name)
			continue
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: no RuntimeDefault seccomp profile", name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: capabilities are not dropped", name)
		}
	}
}
