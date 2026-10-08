/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The table is what keeps five acts agreeing about what an app owns, so it
// has to be complete: a kind that provisioning makes must say what an export
// does with it and what each teardown does with it. Adding a kind to
// AppKinds -- or a field to Stores, which is how a new kernel store is
// declared -- without doing so fails here.
func TestEveryKindProvisioningMakesIsAccountedForByExportAndTeardown(t *testing.T) {
	seen := map[Kind]bool{}
	for _, rule := range AppKinds {
		if rule.Kind == "" || seen[rule.Kind] {
			t.Fatalf("kind %q is empty or listed twice", rule.Kind)
		}
		seen[rule.Kind] = true
		if rule.MadeBy == "" {
			t.Errorf("%s: nothing says what makes it", rule.Kind)
		}
		switch rule.Export {
		case Carries:
		case Omits:
			if rule.ExportNote == "" {
				t.Errorf("%s: an export omits it and does not say why", rule.Kind)
			}
		default:
			t.Errorf("%s: an export must either carry it or omit it for a stated reason, not %q", rule.Kind, rule.Export)
		}
		if rule.Uninstall != Keeps && rule.Uninstall != Removes {
			t.Errorf("%s: uninstalling must keep it or remove it, not %q", rule.Kind, rule.Uninstall)
		}
		// What uninstalling keeps, a purge of the app destroys; what it
		// removes is gone by then. There is no third case: something kept
		// that no purge destroys would be kept for ever.
		switch {
		case rule.Uninstall == Keeps && rule.AppPurge != Destroys:
			t.Errorf("%s: uninstalling keeps it and the app's purge does not destroy it", rule.Kind)
		case rule.Uninstall == Removes && rule.AppPurge != Gone:
			t.Errorf("%s: uninstalling removes it, so there is nothing for a purge to do, not %q", rule.Kind, rule.AppPurge)
		}
		// What uninstalling keeps has to be findable afterwards by something
		// that does not expire: a MariaDB database, a bucket and a cache user
		// used to be found only through their setup Jobs, and a tenant
		// deleted after those were gone left them behind.
		if rule.Uninstall == Keeps && rule.FoundBy == "" {
			t.Errorf("%s: uninstalling keeps it and nothing says how it is found afterwards", rule.Kind)
		}
		if rule.TenantDelete != Destroys && rule.TenantDelete != Removes {
			t.Errorf("%s: deleting the tenant must destroy or remove it, not %q", rule.Kind, rule.TenantDelete)
		}
	}

	// Every store a profile can declare is a kind with a destroy Job: a new
	// field on Stores that StoreDestroyJobs does not know leaves this short.
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	declared := reflect.TypeOf(Stores{}).NumField()
	all := Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL, S3: true, Redis: true}
	jobs := StoreDestroyJobs(tenant, "wiki", all, DestroyWithinARequest)
	if len(jobs) != declared {
		t.Fatalf("a profile can declare %d kinds of store and %d have a destroy Job: %v", declared, len(jobs), jobs)
	}
	// And every one of them is written down when it is provisioned: a store
	// the record leaves out is one nothing finds once its app is uninstalled.
	recorded := ProvisionedOf(InventoryOf(tenant, "wiki", allStoresProfile()))
	if recorded.Stores() != all {
		t.Fatalf("the record of what was provisioned says %+v for a profile that declares %+v", recorded.Stores(), all)
	}
	for kind := range jobs {
		if !recorded.Has(kind) {
			t.Errorf("%s has a destroy Job and the record of what was provisioned does not name it", kind)
		}
		if recorded.Without(kind).Has(kind) {
			t.Errorf("%s cannot be taken out of the record once it is destroyed", kind)
		}
		if !seen[kind] {
			t.Errorf("the destroy Job for %s is of a kind the table does not list", kind)
		}
		if RuleFor(kind).AppPurge != Destroys || RuleFor(kind).TenantDelete != Destroys {
			t.Errorf("%s has a destroy Job and the table does not say it is destroyed", kind)
		}
	}
}

// One order: teardown is provisioning reversed, and the purge of an app is
// the part of it that destroys.
func TestTeardownIsProvisioningReversed(t *testing.T) {
	made, torn := ProvisionOrder(), TeardownOrder()
	reversed := slices.Clone(made)
	slices.Reverse(reversed)
	if !reflect.DeepEqual(torn, reversed) {
		t.Fatalf("teardown %v is not provisioning %v reversed", torn, made)
	}
	want := []Kind{KindFiles, KindModelAccess, KindCache, KindObjectStorage, KindDatabase, KindSignInScope, KindAccessGroup, KindCredentials, KindRecords}
	if got := AppPurgeOrder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("an app's purge works in the order %v, want %v", got, want)
	}
	// The stores are made with the passwords the vault already holds, and
	// the records are written from the first step on.
	if made[0] != KindRecords || made[1] != KindCredentials {
		t.Errorf("provisioning starts with %v", made[:2])
	}
}

// The names of what an app owns, pinned: provisioning creates them, export
// reads them and both teardowns destroy them, and none spells one out again.
func TestTheInventoryNamesWhatAnAppOwns(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "docmost-ce"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{Chart: &gentianov1alpha1.ChartRef{Name: "docmost"}},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL},
				Storage:  &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}},
				Cache:    &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis},
			}},
			Extensions: []gentianov1alpha1.AppSidecarSpec{{Name: "mcp"}},
		},
	}
	got := InventoryOf(tenant, "docmost-ce", profile)
	want := AppInventory{
		Tenant: "demo", App: "docmost-ce",
		Stores:         Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL, S3: true, Redis: true},
		Database:       "demo_docmost_ce",
		DatabaseUser:   "demo_docmost-ce",
		DatabaseRecord: "db-demo-docmost-ce",
		Bucket:         "demo-docmost-ce",
		CacheUser:      "demo-docmost-ce",
		Keys:           []string{"docmost-ce", "docmost-ce-mcp"},
		Releases:       []string{"docmost-ce-release", "tenant-demo-docmost-ce", "docmost-ce-mcp-release"},
		Chart:          "docmost",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory =\n%+v\nwant\n%+v", got, want)
	}

	maria := profile.DeepCopy()
	maria.Spec.Requires.Services.Database.Engine = gentianov1alpha1.DatabaseEngineMariaDB
	if inv := InventoryOf(tenant, "docmost-ce", maria); inv.DatabaseUser != "demo_docmost_ce" || inv.DatabaseRecord != "" {
		t.Errorf("a MariaDB app: user %q, record %q", inv.DatabaseUser, inv.DatabaseRecord)
	}
	// Without a profile only what every app has is named.
	bare := InventoryOf(tenant, "docmost-ce", nil)
	if bare.Database != "" || bare.Bucket != "" || bare.CacheUser != "" || len(bare.Keys) != 1 {
		t.Errorf("an inventory without a profile names stores nothing declared: %+v", bare)
	}
}

func destroyScripts() map[string]string {
	return map[string]string{
		"postgres": postgresDestroyScript("demo_wiki", "demo_wiki"),
		"mariadb":  mariadbDestroyScript("demo_wiki", "demo_wiki"),
		"minio":    objectStorageDestroyScript("demo-wiki"),
		"redis":    cacheDestroyScript("demo-wiki"),
	}
}

// The destroy scripts end in success only when what they were asked to
// remove is verifiably gone: they stop at the first failing command and
// discard no failure. And they have to parse -- a Job that fails at `sh -n`
// strands a deletion on a syntax error nobody sees until a purge.
func TestTheDestroyScriptsDiscardNoFailure(t *testing.T) {
	for name, script := range destroyScripts() {
		if !strings.HasPrefix(script, "set -eu") {
			t.Errorf("%s: the script does not stop at the first failing command", name)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") {
				t.Errorf("%s: a failure is discarded: %s", name, line)
			}
			if strings.Contains(line, "2>/dev/null") && !strings.Contains(line, "user info") {
				t.Errorf("%s: an error is hidden: %s", name, line)
			}
		}
		shell := "sh"
		if strings.Contains(script, "pipefail") {
			shell = "bash"
		}
		if out, err := exec.Command(shell, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %s -n: %v\n%s", name, shell, err, out)
		}
	}
	scripts := destroyScripts()
	for _, want := range []string{
		`format('DROP DATABASE IF EXISTS %I', :'db')`, `DROP OWNED BY \"demo_wiki\"`, `DROP ROLE IF EXISTS \"demo_wiki\";`,
		// Which databases are the app's is the inventory's one question.
		postgresOwnedSQL, `-v app_role="${ROLE}"`, "ROLE='demo_wiki'", "DB='demo_wiki'",
		"ON_ERROR_STOP=1", "is still there",
	} {
		if !strings.Contains(scripts["postgres"], want) {
			t.Errorf("the PostgreSQL script is missing %q", want)
		}
	}
	// The user and policy are found through the policy statement that names
	// the bucket exactly -- a prefix match would take a sibling bucket's
	// user with it.
	for _, want := range []string{`mc rb --force "gentian/demo-wiki"`, "mc admin user rm", `mc admin policy rm gentian "${policy}"`, `arn:aws:s3:::demo-wiki"`} {
		if !strings.Contains(scripts["minio"], want) {
			t.Errorf("the object-storage script is missing %q", want)
		}
	}
	if !strings.Contains(scripts["redis"], "ACL DELUSER 'demo-wiki'") || !strings.Contains(scripts["redis"], "ACL LIST") {
		t.Error("the cache script does not remove the user and then look for it")
	}
}

// One Job per store, the same for an app's purge and a tenant's deletion but
// for how long it may run: named and labelled so both can find it, in the
// store's own namespace, under the restricted context those namespaces
// enforce, and failing rather than retrying for ever.
func TestTheDestroyJobs(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	cases := []struct {
		job       *batchv1.Job
		name, ns  string
		mustNamed string
	}{
		{PostgresDestroyJob(tenant, "wiki", DestroyWithinARequest), "pg-delete-demo-wiki", "system-postgresql", "demo_wiki"},
		{MariaDBDestroyJob(tenant, "wiki", DestroyWithinARequest), "mariadb-delete-demo-wiki", "system-mariadb", "DROP DATABASE"},
		{ObjectStorageDestroyJob(tenant, "wiki", DestroyWithinARequest), "s3-delete-demo-wiki", "system-s3", "gentian/demo-wiki"},
		{CacheDestroyJob(tenant, "wiki", DestroyWithinARequest), "redis-acl-delete-demo-wiki", "system-cache", "demo-wiki"},
	}
	for _, c := range cases {
		job := c.job
		if job.Name != c.name || job.Namespace != c.ns {
			t.Errorf("job %s/%s, want %s/%s", job.Namespace, job.Name, c.ns, c.name)
		}
		if job.Labels["gentianos.io/tenant"] != "demo" || job.Labels["gentianos.io/app"] != "wiki" || job.Labels["app.kubernetes.io/managed-by"] != "gentian-os" {
			t.Errorf("%s: labels %v", c.name, job.Labels)
		}
		spec := job.Spec
		if spec.BackoffLimit == nil || *spec.BackoffLimit != 1 || spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("%s: a destroy Job that cannot do its work must fail, not retry for ever", c.name)
		}
		if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 100 {
			t.Errorf("%s: deadline %v", c.name, spec.ActiveDeadlineSeconds)
		}
		container := spec.Template.Spec.Containers[0]
		sc := container.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.SeccompProfile == nil || sc.Capabilities == nil {
			t.Errorf("%s: the pod would be refused admission in %s", c.name, c.ns)
		}
		if !strings.Contains(strings.Join(container.Command, " "), c.mustNamed) {
			t.Errorf("%s: the script does not name %q", c.name, c.mustNamed)
		}
	}
	// A tenant's deletion is not waited for by anybody and gives the same
	// Job the time a bucket takes to empty.
	if got := *ObjectStorageDestroyJob(tenant, "wiki", DestroyInTheBackground).Spec.ActiveDeadlineSeconds; got != 3600 {
		t.Errorf("background deadline = %d", got)
	}
	// The backup bucket is deleted as its own unit, and the unit's name
	// resolves to exactly the bucket exports write to.
	backupJob := ObjectStorageDestroyJob(tenant, "gentian-backup", DestroyInTheBackground)
	if !strings.Contains(strings.Join(backupJob.Spec.Template.Spec.Containers[0].Command, " "), "gentian/"+BackupBucket(tenant)) {
		t.Error("the backup bucket's unit does not name the bucket exports write to")
	}
}

// allStoresProfile declares every kernel store a profile can.
func allStoresProfile() *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL},
				Storage:  &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}},
				Cache:    &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis},
			}},
		},
	}
}

// Which databases are an app's is one rule, and the three acts that need it
// ask the same question: an export copies the databases the app's role owns,
// a restore puts them back and a purge drops them. An export used to copy
// the provisioned database alone, so a purge destroyed more than a bundle
// held.
func TestExportRestoreAndPurgeAgreeOnWhichDatabasesAreAnApps(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	role, db := PostgresRole("demo", "wiki"), DatabaseName(tenant, "wiki")
	p := JobParams{Namespace: "system-s3", Name: "j", Tenant: "demo", App: "wiki", Bucket: "b", Prefix: "p",
		Encryption: Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{"age1qqqsyqcyq5rqwzqfpg9scrgwpugpzysnzs23v9ccrydpk8qarc0sxpzkh"}}}
	d := Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, SecretName: "k", SecretKey: "identity"}

	scripts := map[string]string{
		"purge":   PostgresDestroyJob(tenant, "wiki", DestroyWithinARequest).Spec.Template.Spec.Containers[0].Command[2],
		"export":  containerByName(PostgresOwnedDumpJob(p, role, db), "pg-dump-owned").Args[0],
		"restore": containerByName(PostgresOwnedRestoreJob(p, d, PostgresOwnedArtefact(db), db, db), "pg-restore-owned").Args[0],
	}
	for act, script := range scripts {
		if !strings.Contains(script, postgresOwnedSQL) {
			t.Errorf("%s does not ask the inventory's question about which databases the role owns", act)
		}
		if !strings.Contains(script, "ROLE='demo_wiki'") || !strings.Contains(script, "DB='demo_wiki'") {
			t.Errorf("%s does not ask it of the app's role and database:\n%s", act, script)
		}
		shell := "sh"
		if strings.Contains(script, "pipefail") {
			shell = "bash"
		}
		if out, err := exec.Command(shell, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %s -n: %v\n%s", act, shell, err, out)
		}
		// A name is the tenant's own choice: it reaches the server as a
		// variable the server quotes, never spliced into SQL or split on
		// spaces.
		if strings.Contains(script, "for db in") {
			t.Errorf("%s splits database names on white space", act)
		}
	}
	// The question leaves out the provisioned database, which each act
	// handles by name, and templates, which are nobody's.
	for _, want := range []string{"d.datname <> :'app_db'", "r.rolname = :'app_role'", "NOT d.datistemplate"} {
		if !strings.Contains(postgresOwnedSQL, want) {
			t.Errorf("the ownership question lacks %q", want)
		}
	}
	// The export files the archive under the provisioned database's name,
	// where the restore's manifest entry points.
	if got := PostgresOwnedArtefact(db); got != "postgres/demo_wiki.owned.tar.gz" {
		t.Errorf("artefact = %q", got)
	}
	// A restore puts back what the archive holds and leaves what it does
	// not: it creates and loads, and drops no database.
	if strings.Contains(scripts["restore"], "DROP DATABASE") {
		t.Error("a restore drops a database the bundle does not hold")
	}
	if !strings.Contains(scripts["restore"], "CREATE DATABASE %I OWNER %I") {
		t.Error("a restore does not create a database the archive holds and the server lacks")
	}

}

// A restore of a bucket makes the bucket's user and policy first, with the
// container install makes them with, and only then writes the objects.
func TestABucketIsProvisionedBeforeItsObjectsAreRestored(t *testing.T) {
	p := JobParams{Namespace: "system-s3", Name: "j", Tenant: "demo", App: "wiki", Bucket: "b", Prefix: "p"}
	d := Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, SecretName: "k", SecretKey: "identity"}
	install := ObjectStorageProvisionContainer("create-bucket", "demo-wiki", "AK", "SK")
	provision := ObjectStorageProvisionContainer("provision-bucket", "demo-wiki", "AK", "SK")
	job := S3RestoreJob(p, d, S3Artefact("old-wiki"), "demo-wiki", provision)

	inits := job.Spec.Template.Spec.InitContainers
	last := inits[len(inits)-1]
	if last.Name != "provision-bucket" {
		t.Fatalf("the last step before the objects are written is %q, want the bucket's provisioning", last.Name)
	}
	// The same code, not a copy: the script and the environment are
	// install's.
	if last.Command[2] != install.Command[2] || !reflect.DeepEqual(last.Env, install.Env) {
		t.Error("a restore provisions the bucket with something other than what install runs")
	}
	for _, want := range []string{"mc admin user add", "mc admin policy create", "mc admin policy attach", `arn:aws:s3:::demo-wiki`} {
		if !strings.Contains(last.Command[2], want) {
			t.Errorf("provisioning lacks %q", want)
		}
	}
	// The artefact is the bundle's name for it and the bucket is the
	// tenant's: two names.
	fetch := containerByName(job, "fetch").Args[0]
	if !strings.Contains(fetch, "s3/old-wiki.tar.gz") {
		t.Errorf("the artefact fetched is not the one the manifest names:\n%s", fetch)
	}
	write := containerByName(job, "s3-restore").Args[0]
	if !strings.Contains(write, `"platform/demo-wiki"`) || strings.Contains(write, "old-wiki") {
		t.Errorf("the objects are not written to the tenant's bucket:\n%s", write)
	}
	if strings.Contains(write, "mc mb") {
		t.Error("the restore makes the bucket a second time, by itself")
	}
}

// What a tenant has that is no app's, and lies outside what a deletion
// sweeps by default, has to say what removes it under each policy. The mail
// records, the client in the kernel realm and the model gateway's team were
// each left behind for want of exactly that.
func TestWhatATenantOwnsSaysWhatItsDeletionDoesWithIt(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range TenantOwned {
		if rule.What == "" || seen[rule.What] {
			t.Fatalf("%q is empty or listed twice", rule.What)
		}
		seen[rule.What] = true
		for field, value := range map[string]string{"madeBy": rule.MadeBy, "export": rule.Export, "retain": rule.Retain, "delete": rule.Delete} {
			if value == "" {
				t.Errorf("%s: nothing is said for %s", rule.What, field)
			}
		}
	}
	all := ""
	for _, rule := range TenantOwned {
		all += rule.What + "\n"
	}
	for _, must := range []string{"kernel realm", "model gateway", "mail DNS records", "vault subtree", "namespace", "backup bucket", "record of what was provisioned"} {
		if !strings.Contains(all, must) {
			t.Errorf("the list of what a tenant owns does not name its %s", must)
		}
	}
}

// The model key is on record like a store: found after an uninstall, and
// taken off when it is removed.
func TestTheModelKeyIsOnRecord(t *testing.T) {
	p := Provisioned{ModelKey: "demo-wiki"}
	if p.Empty() || !p.Has(KindModelAccess) || p.Without(KindModelAccess).Has(KindModelAccess) || !p.Without(KindModelAccess).Empty() {
		t.Errorf("record entry = %+v", p)
	}
	// It is no kernel store: it adds nothing to what a destroy Job is run for.
	if p.Stores() != (Stores{}) {
		t.Errorf("stores = %+v", p.Stores())
	}
	record := NewProvisionedRecord("demo")
	if _, err := RecordProvisioned(record, "wiki", Provisioned{Bucket: "demo-wiki"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordProvisioned(record, "wiki", p); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadProvisioned(record)
	if got["wiki"].ModelKey != "demo-wiki" || got["wiki"].Bucket != "demo-wiki" {
		t.Errorf("recorded = %+v", got["wiki"])
	}
}
