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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// serverClient is one row of the `servers` section of
// scripts/tests/store-clients.yaml: a client of the kernel's own PostgreSQL.
type serverClient struct {
	Server    string            `json:"server"`
	Client    string            `json:"client"`
	Namespace any               `json:"namespace"`
	PodLabels map[string]string `json:"podLabels"`
	Port      int32             `json:"port"`
	Built     string            `json:"built"`
	Evidence  string            `json:"evidence"`
}

// The kernel's own PostgreSQL admits a connection by where it comes from
// (kernel/data/kernel-postgres/templates/networkpolicy.yaml). Its clients are
// written down beside the stores', and the policy is tested against that
// file. This holds the file to the operator's code, for the client the
// operator's code decides: the desktop of the tenant that adopts the kernel
// realm, whose database is on the kernel's server and nowhere else. The
// clients a chart builds are held to the charts by
// scripts/tests/server_network_policies.py.
func TestTheKernelDatabaseClientsAreWhereItsNetworkPolicyExpectsThem(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "tests", "store-clients.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Servers struct {
			Clients []serverClient `json:"clients"`
		} `json:"servers"`
	}
	if err := yaml.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Servers.Clients) == 0 {
		t.Fatal("the file lists no client of the kernel's PostgreSQL")
	}

	const kernelRealm = "kernel"
	// The tenant whose realm is the kernel's, whatever it is called, and one
	// that has a realm of its own.
	platform := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec:       gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: kernelRealm}},
	}
	acme := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	desktopProfile := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
			Database: &gentianov1alpha1.DatabaseRequirement{}}},
	}}
	r := &ComponentReconciler{KernelRealm: kernelRealm}
	desktop := func(tenant *gentianov1alpha1.Tenant) *gentianov1alpha1.Component {
		return &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: tenant.NamespaceName()}}
	}
	built := map[string]func(*testing.T) builtClient{
		"kernel-realm-desktop-database": func(t *testing.T) builtClient {
			np := buildComponentNetworkPolicy(desktop(platform), r.componentEgressNamespaces(desktopProfile, platform), nil)
			return egressClient(t, np, layout.Namespace(layout.Data), provisioner.PostgresPort)
		},
	}

	// Where each server is, by the layout the operator itself uses.
	serverNamespace := map[string]string{
		"kernel-postgres": layout.Namespace(layout.Data),
	}

	seen := map[string]bool{}
	for _, c := range table.Servers.Clients {
		if _, known := serverNamespace[c.Server]; !known {
			t.Errorf("%s: %q is not a server this test knows", c.Client, c.Server)
			continue
		}
		if c.Evidence != "proven" && c.Evidence != "inferred" {
			t.Errorf("%s: evidence is %q", c.Client, c.Evidence)
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
		got := build(t)
		row := storeClient{Client: c.Client, Namespace: c.Namespace}
		if want := row.namespaceName(t); got.namespace != want {
			t.Errorf("%s: runs in %s, the file says %s", c.Client, got.namespace, want)
		}
		for k, v := range c.PodLabels {
			if got.podLabels[k] != v {
				t.Errorf("%s: its pods carry %v, the file says %s=%s", c.Client, got.podLabels, k, v)
			}
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

	// The desktop's database is on the kernel's server for the tenant that
	// adopts the kernel realm and for no other: the rule admits tenant
	// namespaces because that tenant is found by its realm, and what keeps
	// every other tenant out is that nothing opens the way for it.
	if got := r.componentDatabaseNamespace(platform); got != layout.Namespace(layout.Data) {
		t.Errorf("the desktop of the tenant that adopts the kernel realm keeps its database in %s", got)
	}
	if got := r.componentDatabaseNamespace(acme); got == layout.Namespace(layout.Data) {
		t.Error("a tenant with a realm of its own keeps its desktop's database on the kernel's server")
	}
	for _, ns := range r.componentEgressNamespaces(desktopProfile, acme) {
		if ns == layout.Namespace(layout.Data) {
			t.Error("the desktop of a tenant with a realm of its own is opened the kernel's data namespace")
		}
	}
	if got := layout.TenantLabels(platform.Name, false); got[layout.LabelTier] != "tenant" {
		t.Errorf("the namespace of the tenant that adopts the kernel realm is labelled %v", got)
	}

	// The ports the file gives are the ones the code hands out.
	for _, c := range table.Servers.Clients {
		if c.Server == "kernel-postgres" && !strings.HasPrefix(c.Client, "CloudNativePG") && c.Port != provisioner.PostgresPort {
			t.Errorf("%s: the file says port %d, PostgreSQL answers on %d", c.Client, c.Port, provisioner.PostgresPort)
		}
		// The operator and the registrar are in the control namespace, and a
		// row that names a kernel namespace names the one the layout has.
		if component := c.PodLabels["app.kubernetes.io/component"]; component == "operator" || component == "registrar" {
			if ns, _ := c.Namespace.(string); ns != layout.Namespace(layout.Control) {
				t.Errorf("%s: the %s runs in %s", c.Client, component, layout.Namespace(layout.Control))
			}
		}
	}
}
