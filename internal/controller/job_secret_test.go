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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

// passwordEnvOf is how a container is handed a password: the Secret and key
// it reads it from, or the literal value it carries.
func passwordEnvOf(t *testing.T, job *batchv1.Job, env string) (ref, literal string) {
	t.Helper()
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name != env {
			continue
		}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key, e.Value
		}
		return "", e.Value
	}
	t.Fatalf("Job %s has no %s", job.Name, env)
	return "", ""
}

// An app's PostgreSQL role password, MariaDB user password and cache user
// password are each handed to the Job that provisions the store in a Secret
// the Job owns, and are in nothing else the operator writes: not in the
// Job's spec, not in the provisioning ConfigMap Crossplane composes the Job
// from.
func TestTheStoreJobsReadTheAppsPasswordFromASecretTheyOwn(t *testing.T) {
	ctx := context.Background()
	t.Setenv("POSTGRES_PROVISIONER_IMAGE", "")
	t.Setenv("MARIADB_PROVISIONER_IMAGE", "")
	t.Setenv("REDIS_PROVISIONER_IMAGE", "")
	profile := func(name string, req gentianov1alpha1.ServiceRequirements) *gentianov1alpha1.ComponentProfile {
		return &gentianov1alpha1.ComponentProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gentianov1alpha1.ComponentProfileSpec{
				Requires: &gentianov1alpha1.RequirementSpec{Services: &req},
			},
		}
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{
			{Profile: "wiki"}, {Profile: "shop"}, {Profile: "chat"},
		}},
	}
	redisAdmin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: redisAdminSecret, Namespace: cacheNamespace},
		Data:       map[string][]byte{"host": []byte("redis.cache.svc")},
	}
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		profile("wiki", gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL}}),
		profile("shop", gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB}}),
		profile("chat", gentianov1alpha1.ServiceRequirements{Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}}),
		tenant, redisAdmin,
	).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Scheme: s, Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}

	jobs, keys, err := r.buildDataPlaneJobs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := serializeProvisioningJobs(jobs)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := r.buildDataPlaneObjects(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	objectsPayload, err := serializeProvisioningObjects(objects)
	if err != nil {
		t.Fatal(err)
	}

	for _, store := range []struct {
		name, vaultPath, env string
		job                  types.NamespacedName
	}{
		{"postgres role", secrets.CategoryPath("demo", "wiki", "database"), "ROLE_PW",
			types.NamespacedName{Namespace: postgresNamespace, Name: roleJobName("demo", "wiki")}},
		{"mariadb user", secrets.CategoryPath("demo", "shop", "database"), "DB_PASS",
			types.NamespacedName{Namespace: mariadbNamespace, Name: mariadbSetupJobName("demo", "shop")}},
		{"cache user", secrets.CategoryPath("demo", "chat", "cache"), "REDIS_USER_PASSWORD",
			types.NamespacedName{Namespace: cacheNamespace, Name: redisACLJobName("demo", "chat")}},
	} {
		t.Run(store.name, func(t *testing.T) {
			password := vault[store.vaultPath]["password"]
			if password == "" {
				t.Fatalf("no password was seeded at %s: %v", store.vaultPath, vault)
			}
			if strings.Contains(payload, password) || strings.Contains(objectsPayload, password) {
				t.Fatal("the provisioning manifests carry the app's password")
			}
			var built *batchv1.Job
			for i := range jobs {
				if jobs[i].Name == store.job.Name && jobs[i].Namespace == store.job.Namespace {
					built = &jobs[i]
				}
			}
			if built == nil {
				t.Fatalf("no Job %s among %d", store.job, len(jobs))
			}
			ref, literal := passwordEnvOf(t, built, store.env)
			if literal != "" || ref != store.job.Name+"/password" {
				t.Errorf("%s is not read from the Job's own Secret: ref %q, literal %q", store.env, ref, literal)
			}
			if string(keys[store.job]["password"]) != password {
				t.Fatal("the Job is to be handed another password than the vault's")
			}

			// Before Crossplane has made the Job there is nothing to own the Secret.
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			sec := &corev1.Secret{}
			if err := c.Get(ctx, store.job, sec); !apierrors.IsNotFound(err) {
				t.Fatalf("a Secret was made before its Job (%v)", err)
			}

			// The Job as Crossplane makes it.
			made := built.DeepCopy()
			made.UID = "job-uid-1"
			if err := c.Create(ctx, made); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, store.job, sec); err != nil {
				t.Fatalf("no Secret beside the Job: %v", err)
			}
			if string(sec.Data["password"]) != password {
				t.Error("the Secret holds another password than the vault's")
			}
			if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" ||
				sec.OwnerReferences[0].Name != store.job.Name || sec.OwnerReferences[0].UID != "job-uid-1" {
				t.Errorf("the Secret is not owned by its Job: %+v", sec.OwnerReferences)
			}
			if sec.Labels[tenantLabel] != "demo" || sec.Labels[appLabel] != made.Labels[appLabel] {
				t.Errorf("the Secret does not carry its Job's labels: %v", sec.Labels)
			}

			// A changed password is written over the one there.
			changed := provisioningJobSecrets{store.job: passwordSecretData("another")}
			if err := r.ensureProvisioningJobSecrets(ctx, changed); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, store.job, sec); err != nil || string(sec.Data["password"]) != "another" {
				t.Errorf("the Secret was not rewritten with the changed password (%v)", err)
			}

			// A Job made again (a retired one's Secret went with it) owns it anew.
			if err := c.Delete(ctx, made); err != nil {
				t.Fatal(err)
			}
			again := built.DeepCopy()
			again.UID = "job-uid-2"
			if err := c.Create(ctx, again); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, store.job, sec); err != nil || sec.OwnerReferences[0].UID != "job-uid-2" ||
				string(sec.Data["password"]) != password {
				t.Errorf("the Secret is not the Job made again's (%v): %+v", err, sec.OwnerReferences)
			}

			// A finished Job is handed nothing.
			if err := c.Delete(ctx, sec); err != nil {
				t.Fatal(err)
			}
			again.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := c.Status().Update(ctx, again); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, store.job, sec); !apierrors.IsNotFound(err) {
				t.Errorf("a finished Job was handed a Secret (%v)", err)
			}
		})
	}
}

// Without a vault there is no password to hand over: the Jobs read none and
// nothing is to be handed to them.
func TestTheStoreJobsReadNoPasswordWithoutAVault(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	for env, job := range map[string]*batchv1.Job{
		"ROLE_PW":             makeRoleJob(tenant, "tenant-demo", "demo_wiki", "wiki", false, "", false),
		"DB_PASS":             makeMariaDBSetupJob(tenant, "shop", false, false),
		"REDIS_USER_PASSWORD": makeRedisACLJob(tenant, "chat", false),
	} {
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == env {
				t.Errorf("%s carries %s without a vault", job.Name, env)
			}
		}
	}
}

// The desktop's database role is made by the operator itself, and on a
// restore by the same code: its password reaches the role Job in a Secret
// the Job owns, never in the Job.
func TestTheDesktopsRoleJobReadsItsPasswordFromASecretItOwns(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	vault := memVault{}
	seeder := secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))
	tenant := acmeTenantFixture()
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(cnpgClusterGVK)
	pg.SetName(cnpgClusterName)
	pg.SetNamespace(postgresNamespace)
	if err := c.Create(ctx, pg); err != nil {
		t.Fatal(err)
	}
	step := func() {
		t.Helper()
		if _, _, _, err := ensurePostgresStore(ctx, c, seeder, tenant, portalShellAppName, gentianov1alpha1.SchemaPreferenceAppSchema, false); err != nil {
			t.Fatal(err)
		}
	}
	name := types.NamespacedName{Name: roleJobName("acme", portalShellAppName), Namespace: postgresNamespace}

	step()
	password := vault[secrets.CategoryPath("acme", portalShellAppName, "database")]["password"]
	job := &batchv1.Job{}
	if err := c.Get(ctx, name, job); err != nil {
		t.Fatal(err)
	}
	if ref, literal := passwordEnvOf(t, job, "ROLE_PW"); literal != "" || ref != name.Name+"/password" {
		t.Fatalf("ROLE_PW is not read from the Job's own Secret: ref %q, literal %q", ref, literal)
	}
	// The Job as the cluster has it, with its UID; the next pass hands it
	// its Secret.
	job.UID = "role-job-uid"
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	step()
	sec := &corev1.Secret{}
	if err := c.Get(ctx, name, sec); err != nil {
		t.Fatalf("no Secret beside the role Job: %v", err)
	}
	if password == "" || string(sec.Data["password"]) != password {
		t.Error("the Secret holds another password than the vault's")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != "role-job-uid" {
		t.Errorf("the Secret is not owned by the role Job: %+v", sec.OwnerReferences)
	}
}

// A restore that puts back a MariaDB app runs install's setup Job, and the
// user's password reaches it the same way: a Secret the Job owns.
func TestARetainedMariaDBSetupReadsItsPasswordFromASecretItOwns(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	vault := memVault{}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	r := &TenantRestoreReconciler{Client: c, Scheme: s,
		Tenant: &TenantReconciler{Client: c, Scheme: s, Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}}
	name := types.NamespacedName{Name: mariadbSetupJobName("demo", "shop"), Namespace: mariadbNamespace}

	if _, err := r.ensureRetainedMariaDB(ctx, tenant, "shop", true, false); err != nil {
		t.Fatal(err)
	}
	password := vault[secrets.CategoryPath("demo", "shop", "database")]["password"]
	job := &batchv1.Job{}
	if err := c.Get(ctx, name, job); err != nil {
		t.Fatal(err)
	}
	if ref, literal := passwordEnvOf(t, job, "DB_PASS"); literal != "" || ref != name.Name+"/password" {
		t.Fatalf("DB_PASS is not read from the Job's own Secret: ref %q, literal %q", ref, literal)
	}
	job.UID = "setup-job-uid"
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if done, err := r.ensureRetainedMariaDB(ctx, tenant, "shop", false, false); err != nil || done {
		t.Fatalf("done=%v err=%v while the Job runs", done, err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), sec); err != nil {
		t.Fatalf("no Secret beside the setup Job: %v", err)
	}
	if password == "" || string(sec.Data["password"]) != password {
		t.Error("the Secret holds another password than the vault's")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != "setup-job-uid" {
		t.Errorf("the Secret is not owned by the setup Job: %+v", sec.OwnerReferences)
	}
}

// assertClientJobReadsItsOwnSecret is what each Keycloak client Job builder
// is held to: the Job reads the app's OIDC client secret from the Secret it
// owns, and its manifest -- env and script -- carries no literal one.
func assertClientJobReadsItsOwnSecret(t *testing.T, job *batchv1.Job, clientSecret string) {
	t.Helper()
	raw, err := serializeProvisioningJobs([]batchv1.Job{*job})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, clientSecret) {
		t.Fatal("the provisioning manifests carry the app's client secret")
	}
	ref, literal := passwordEnvOf(t, job, "OIDC_CLIENT_SECRET")
	if literal != "" || ref != job.Name+"/"+jobSecretClientSecretKey {
		t.Errorf("OIDC_CLIENT_SECRET is not read from the Job's own Secret: ref %q, literal %q", ref, literal)
	}
}

// An app's OIDC client secret is handed to the Job that makes its client in
// a Secret the Job owns, and is in nothing else the operator writes: not in
// the Job's spec, not in the provisioning ConfigMap Crossplane composes the
// Job from. Both builders: the pack Job and the plain client Job.
func TestTheClientJobsReadTheAppsClientSecretFromASecretTheyOwn(t *testing.T) {
	ctx := context.Background()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	s := componentDatabaseScheme(t)
	for name, cfg := range map[string]oidcAppConfig{
		"oidc pack": {profileName: "wiki", clientID: "demo-wiki", redirectURIs: []string{"https://wiki.demo.example.test/*"},
			pack: &oidc.Pack{ScopeName: "wiki-scope", ClientRole: "access"}},
		"oidc client": {profileName: "wiki", clientID: "demo-wiki", redirectURIs: []string{"https://wiki.demo.example.test/*"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).Build()
			vault := memVault{}
			r := &TenantReconciler{Client: c, Scheme: s, KernelDomain: "example.test",
				Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}

			built, clientSecret, err := r.buildOIDCClientProvisioningJob(ctx, tenant, "demo", cfg)
			if err != nil {
				t.Fatal(err)
			}
			if clientSecret == "" || vault[secrets.CategoryPath("demo", "wiki", "oidc")]["client-secret"] != clientSecret {
				t.Fatalf("the Job is to be handed another client secret than the vault's: %q, %v", clientSecret, vault)
			}
			assertClientJobReadsItsOwnSecret(t, built, clientSecret)

			key := types.NamespacedName{Namespace: built.Namespace, Name: built.Name}
			if key.Namespace != identityNamespace || key.Name != clientJobName("demo", "wiki") {
				t.Fatalf("the client Job is %s", key)
			}
			keys := provisioningJobSecrets{key: clientSecretSecretData(clientSecret)}

			// Before Crossplane has made the Job there is nothing to own the Secret.
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			sec := &corev1.Secret{}
			if err := c.Get(ctx, key, sec); !apierrors.IsNotFound(err) {
				t.Fatalf("a Secret was made before its Job (%v)", err)
			}

			made := built.DeepCopy()
			made.UID = "client-job-uid"
			if err := c.Create(ctx, made); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureProvisioningJobSecrets(ctx, keys); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, sec); err != nil {
				t.Fatalf("no Secret beside the Job: %v", err)
			}
			if string(sec.Data[jobSecretClientSecretKey]) != clientSecret {
				t.Error("the Secret holds another client secret than the vault's")
			}
			if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" ||
				sec.OwnerReferences[0].Name != key.Name || sec.OwnerReferences[0].UID != "client-job-uid" {
				t.Errorf("the Secret is not owned by its Job: %+v", sec.OwnerReferences)
			}
		})
	}
}

// A public client, or a cluster without a vault, has no secret to hand over:
// the Job reads none and nothing is to be handed to it.
func TestTheClientJobsReadNoClientSecretWhenThereIsNone(t *testing.T) {
	ctx := context.Background()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	s := componentDatabaseScheme(t)
	uris := []string{"https://wiki.demo.example.test/*"}
	vaulted := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s,
		Seeder: secrets.NewSeeder(memVault{}, secrets.NewDeriver("unit-test-master"))}
	unvaulted := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	for name, c := range map[string]struct {
		r   *TenantReconciler
		cfg oidcAppConfig
	}{
		"public pack":          {vaulted, oidcAppConfig{profileName: "wiki", clientID: "demo-wiki", redirectURIs: uris, pack: &oidc.Pack{PublicClient: true}}},
		"pack without a vault": {unvaulted, oidcAppConfig{profileName: "wiki", clientID: "demo-wiki", redirectURIs: uris, pack: &oidc.Pack{}}},
		"client without vault": {unvaulted, oidcAppConfig{profileName: "wiki", clientID: "demo-wiki", redirectURIs: uris}},
	} {
		job, clientSecret, err := c.r.buildOIDCClientProvisioningJob(ctx, tenant, "demo", c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if clientSecret != "" {
			t.Errorf("%s: a client secret is to be handed over", name)
		}
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "OIDC_CLIENT_SECRET" {
				t.Errorf("%s: the Job carries OIDC_CLIENT_SECRET", name)
			}
		}
	}
}

// The tenant's provisioning: the client secret of an app whose client the
// operator's Job makes is among the credentials handed to the Jobs, and in
// none of the Jobs.
func TestTheTenantsProvisioningHandsTheClientSecretToItsJob(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	api := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{API: &gentianov1alpha1.APIIntegration{}},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{
					ClientID: "demo-wiki", RedirectURIs: []string{"https://wiki.demo.example.test/*"},
				}},
			}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{Profile: "wiki"}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(api, tenant).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Scheme: s, KernelDomain: "example.test",
		Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}

	jobs, keys, err := r.buildTenantProvisioningJobs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	clientSecret := vault[secrets.CategoryPath("demo", "wiki", "oidc")]["client-secret"]
	if clientSecret == "" {
		t.Fatalf("no client secret was seeded: %v", vault)
	}
	payload, err := serializeProvisioningJobs(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, clientSecret) {
		t.Fatal("the provisioning manifests carry the app's client secret")
	}
	key := types.NamespacedName{Namespace: identityNamespace, Name: clientJobName("demo", "wiki")}
	if string(keys[key][jobSecretClientSecretKey]) != clientSecret {
		t.Fatalf("the client Job is not to be handed the vault's client secret: %v", keys[key])
	}
	for i := range jobs {
		if jobs[i].Name == key.Name && jobs[i].Namespace == key.Namespace {
			assertClientJobReadsItsOwnSecret(t, &jobs[i], clientSecret)
			return
		}
	}
	t.Fatalf("no Job %s among %d", key, len(jobs))
}
