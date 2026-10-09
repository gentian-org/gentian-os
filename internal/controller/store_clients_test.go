/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// storeClient is one row of scripts/tests/store-clients.yaml: a pod that
// connects to a shared store, where it runs and what it carries.
type storeClient struct {
	Store     string `json:"store"`
	Client    string `json:"client"`
	Namespace any    `json:"namespace"`
	// PodLabels are the labels a rule of the store's NetworkPolicy may
	// select the client by; the pod may carry more.
	PodLabels map[string]string `json:"podLabels"`
	Port      int32             `json:"port"`
	Built     string            `json:"built"`
}

func (c storeClient) namespaceName(t *testing.T) string {
	t.Helper()
	switch ns := c.Namespace.(type) {
	case string:
		return ns
	case map[string]any:
		name, _ := ns["name"].(string)
		return name
	}
	t.Fatalf("%s: namespace is neither a name nor a name with labels", c.Client)
	return ""
}

// builtClient is what the operator's own code makes of a client.
type builtClient struct {
	namespace string
	podLabels map[string]string
}

func jobClient(job *batchv1.Job) builtClient {
	return builtClient{namespace: job.Namespace, podLabels: job.Spec.Template.Labels}
}

// egressClient is a tenant-side policy read as a client: the pods it selects,
// in its namespace -- provided it opens the store's namespace on the port,
// or whole.
func egressClient(t *testing.T, np *networkingv1.NetworkPolicy, storeNamespace string, port int32) builtClient {
	t.Helper()
	if np == nil {
		t.Fatalf("no policy is built towards %s", storeNamespace)
	}
	reaches := false
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector == nil || to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != storeNamespace {
				continue
			}
			if len(rule.Ports) == 0 {
				reaches = true
			}
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IntVal == port {
					reaches = true
				}
			}
		}
	}
	if !reaches {
		t.Fatalf("policy %s does not open %s on %d", np.Name, storeNamespace, port)
	}
	return builtClient{namespace: np.Namespace, podLabels: np.Spec.PodSelector.MatchLabels}
}

// The shared stores admit a connection by where it comes from
// (templates/networkpolicy.yaml in each engine's chart), and a rule names a
// client by its namespace and its pod labels. Those are written down once,
// in scripts/tests/store-clients.yaml, and the policies are tested against
// that file. This holds the file to the code: every client the operator
// builds runs where the file says and carries what the file says, on the
// port the file says. A Job that moves to another namespace, or a pod
// template that loses the label a rule selects on, fails here -- not as a
// Job that hangs on a cluster until its deadline.
func TestTheStoresClientsAreWhereTheirNetworkPoliciesExpectThem(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "tests", "store-clients.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Clients []storeClient `json:"clients"`
	}
	if err := yaml.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	// Every unit of an export and of a restore, made by the reconcilers' own
	// code, so that where a unit runs is what the reconcilers decide and not
	// what this test assumes: a tenant with wiki (PostgreSQL, a bucket, a
	// volume) and shop (MariaDB).
	world := newPlacementWorld(t, false)
	exports := world.exportUnits(t, gentianov1alpha1.ExportEncryptionRecipient)
	restores := world.restoreUnits(t, gentianov1alpha1.ExportEncryptionRecipient)
	unit := func(units []captureUnit, kind string) func(*testing.T) builtClient {
		return func(t *testing.T) builtClient {
			for _, u := range units {
				if u.Kind == kind {
					return jobClient(u.Job)
				}
			}
			t.Fatalf("no unit of kind %s is built", kind)
			return builtClient{}
		}
	}

	app := func(services gentianov1alpha1.ServiceRequirements, store string, port int32) func(*testing.T) builtClient {
		return func(t *testing.T) builtClient {
			profile := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
				Requires: &gentianov1alpha1.RequirementSpec{Services: &services},
			}}
			np := netpolicy.KernelAccessNetworkPolicy(tenant.Name, tenant.NamespaceName(), profileNameOf(services), profile, netpolicy.Config{})
			return egressClient(t, np, layout.System(store), port)
		}
	}
	job := func(j *batchv1.Job) func(*testing.T) builtClient {
		return func(*testing.T) builtClient { return jobClient(j) }
	}

	// One entry per `built` key of the file, made the way the operator makes it.
	built := map[string]func(*testing.T) builtClient{
		"tenant-app-postgresql": app(gentianov1alpha1.ServiceRequirements{
			Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL}}, "postgresql", provisioner.PostgresPort),
		"tenant-app-mariadb": app(gentianov1alpha1.ServiceRequirements{
			Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB}}, "mariadb", provisioner.MariaDBPort),
		"tenant-app-cache": app(gentianov1alpha1.ServiceRequirements{
			Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}}, "cache", provisioner.RedisPort),
		"tenant-app-s3": app(gentianov1alpha1.ServiceRequirements{
			Storage: &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}}}, "s3", provisioner.ObjectStoragePort),
		"tenant-app-llm": app(gentianov1alpha1.ServiceRequirements{
			LLM: &gentianov1alpha1.LLMRequirement{}}, "llm", provisioner.ModelGatewayPort),
		"tenant-component-database": func(t *testing.T) builtClient {
			comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: tenant.NamespaceName()}}
			profile := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Database: &gentianov1alpha1.DatabaseRequirement{}}},
			}}
			r := &ComponentReconciler{KernelRealm: "kernel"}
			np := buildComponentNetworkPolicy(comp, r.componentEgressNamespaces(profile, tenant), nil)
			return egressClient(t, np, layout.System("postgresql"), provisioner.PostgresPort)
		},
		"tenant-component-llm": func(t *testing.T) builtClient {
			comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: tenant.NamespaceName()}}
			np := buildComponentNetworkPolicy(comp, nil, []networkingv1.NetworkPolicyEgressRule{modelGatewayEgress()})
			return egressClient(t, np, layout.System("llm"), provisioner.ModelGatewayPort)
		},
		"kernel-realm-desktop-llm": func(t *testing.T) builtClient {
			comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "tenant-platform"}}
			np := buildComponentNetworkPolicy(comp, nil, []networkingv1.NetworkPolicyEgressRule{modelGatewayEgress()})
			return egressClient(t, np, layout.System("llm"), provisioner.ModelGatewayPort)
		},
		"postgres-role-job":      job(makeRoleJob(tenant, tenant.NamespaceName(), "acme_wiki", "wiki", "", "", false)),
		"postgres-destroy-job":   job(backup.PostgresDestroyJob(tenant, "wiki", backup.DestroyInTheBackground)),
		"export-postgres-dump":   unit(exports, bundle.ArtefactPostgres),
		"export-postgres-upload": unit(exports, bundle.ArtefactPostgresOwned),
		"restore-postgres":       unit(restores, bundle.ArtefactPostgres),
		"restore-postgres-fetch": unit(restores, bundle.ArtefactPostgresOwned),
		"mariadb-setup-job":      job(makeMariaDBSetupJob(tenant, "shop", "", false)),
		"mariadb-destroy-job":    job(backup.MariaDBDestroyJob(tenant, "shop", backup.DestroyInTheBackground)),
		"export-mariadb-dump":    unit(exports, bundle.ArtefactMariaDB),
		"export-mariadb-upload":  unit(exports, bundle.ArtefactMariaDBOwned),
		"restore-mariadb":        unit(restores, bundle.ArtefactMariaDB),
		"restore-mariadb-fetch":  unit(restores, bundle.ArtefactMariaDBOwned),
		"redis-acl-job":          job(makeRedisACLJob(tenant, "wiki", "")),
		"cache-destroy-job":      job(backup.CacheDestroyJob(tenant, "wiki", backup.DestroyInTheBackground)),
		"s3-bucket-job":          job(makeS3BucketJob(tenant, "wiki", "", "")),
		"s3-destroy-job":         job(backup.ObjectStorageDestroyJob(tenant, "wiki", backup.DestroyInTheBackground)),
		"export-s3-archive":      unit(exports, bundle.ArtefactS3),
		"restore-s3":             unit(restores, bundle.ArtefactS3),
		"export-manifest":        unit(exports, "manifest"),
		"export-volume-archive":  unit(exports, bundle.ArtefactVolume),
		"restore-volume":         unit(restores, bundle.ArtefactVolume),
		"export-realm-upload":    unit(exports, bundle.ArtefactIdentity),
		"restore-realm-fetch":    unit(restores, bundle.ArtefactIdentity),
		// The two units that are not beside a store of the system tier, made
		// as the reconcilers place them: beside the mail server's volume, and
		// beside the kernel's PostgreSQL.
		"export-mailboxes-upload": job(backup.MailboxBackupJob(
			backup.JobParams{Namespace: mailNamespace, Name: "j", Tenant: tenant.Name}, "dovecot-dev-mail", "acme.example")),
		"restore-mailboxes-fetch": job(backup.MailboxRestoreJob(
			backup.JobParams{Namespace: mailNamespace, Name: "j", Tenant: tenant.Name}, backup.Decryption{}, backup.MailboxesArtefact, "dovecot-dev-mail", "acme.example")),
		"export-kernel-desktop-upload": job(backup.KernelDesktopDumpJob(
			backup.JobParams{Namespace: backup.KernelPostgresNamespace(), Name: "j", Tenant: tenant.Name})),
		"restore-kernel-desktop-fetch": job(backup.KernelDesktopRestoreJob(
			backup.JobParams{Namespace: backup.KernelPostgresNamespace(), Name: "j", Tenant: tenant.Name}, backup.Decryption{}, "postgres/portal_shell.pgc")),
	}

	// The data port of each store: what an app is handed and what the
	// tenant's side opens. A client dials no other, bar the one engine
	// operator the file marks as built by nothing here.
	dataPort := map[string]int32{
		"postgresql": provisioner.PostgresPort,
		"mariadb":    provisioner.MariaDBPort,
		"cache":      provisioner.RedisPort,
		"s3":         provisioner.ObjectStoragePort,
		// The model gateway is no store, and is held to the same table: its
		// namespace carries the same kind of policy, per server pod set.
		"llm": provisioner.ModelGatewayPort,
	}

	seen := map[string]bool{}
	for _, c := range table.Clients {
		port, known := dataPort[c.Store]
		if !known {
			t.Errorf("%s: %q is not a shared store", c.Client, c.Store)
			continue
		}
		if c.Built == "none" {
			continue
		}
		build, ok := built[c.Built]
		if !ok {
			t.Errorf("%s: the file says it is built by %q, which this test does not know how to build", c.Client, c.Built)
			continue
		}
		if seen[c.Built] {
			t.Errorf("%s: %q is named by two rows", c.Client, c.Built)
		}
		seen[c.Built] = true
		if c.Port != port {
			t.Errorf("%s: the file says port %d, %s answers on %d", c.Client, c.Port, c.Store, port)
		}
		got := build(t)
		if want := c.namespaceName(t); got.namespace != want {
			t.Errorf("%s: runs in %s, the file says %s", c.Client, got.namespace, want)
		}
		for k, v := range c.PodLabels {
			if got.podLabels[k] != v {
				t.Errorf("%s: its pods carry %v, the file says %s=%s", c.Client, got.podLabels, k, v)
			}
		}
		// A client of the store's own namespace is admitted as such, by a
		// rule that asks for no label; one from anywhere else is admitted by
		// a label, so the file has to name one.
		if got.namespace != layout.System(c.Store) && len(c.PodLabels) == 0 {
			t.Errorf("%s: runs outside %s and the file names no label to admit it by", c.Client, layout.System(c.Store))
		}
	}
	var missing []string
	for name := range built {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("built here and absent from the file, so no policy is tested against them: %v", missing)
	}

	// The operator's own namespace is the one the file gives it.
	for _, c := range table.Clients {
		if c.PodLabels["app.kubernetes.io/component"] == "operator" && c.namespaceName(t) != layout.Namespace(layout.Control) {
			t.Errorf("%s: the operator runs in %s", c.Client, layout.Namespace(layout.Control))
		}
	}
	// And a tenant namespace carries the label the rules admit.
	if got := layout.TenantLabels("acme", false); got[layout.LabelTier] != "tenant" {
		t.Errorf("a tenant namespace is labelled %v", got)
	}
}

// profileNameOf gives each test profile the app name the file uses for it.
func profileNameOf(services gentianov1alpha1.ServiceRequirements) string {
	if services.Database != nil && services.Database.Engine == gentianov1alpha1.DatabaseEngineMariaDB {
		return "shop"
	}
	if services.LLM != nil {
		return "chat"
	}
	return "wiki"
}
