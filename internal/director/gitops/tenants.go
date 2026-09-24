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

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Bringing a tenant on and retiring one, as commits.
//
// This is the errand the console exists for: an MSP employee takes on a
// customer, and what that means here is one directory and one manifest in the
// deployments repository. Argo CD syncs it, the operator provisions the realm,
// the namespaces, the database and the desktop, and the tenant exists.
//
// Nothing in this file talks to the cluster. The director's whole job on the
// write side is to turn an authorised request into a reviewable commit, and a
// tenant is the clearest case of that: the manifest a person would have
// written by hand, written by the thing that checked they were allowed to.

// ErrTenantExists is a create against a name the cluster already has.
var ErrTenantExists = errors.New("tenant already exists")

// ErrTenantProtected is a retire against a tenant that must not be retired.
var ErrTenantProtected = errors.New("tenant is protected")

// Tenant is what a listing says about one tenant.
//
// Read from the manifest rather than from the cluster, because the manifest is
// what the director is responsible for. A tenant whose manifest exists but
// which the operator has not finished provisioning is still a tenant here, and
// the console shows it as committed rather than pretending it is not there.
type Tenant struct {
	Name string `json:"name"`
	// DisplayName is what a person called it. Empty falls back to the name.
	DisplayName string `json:"displayName,omitempty"`
	// Realm is the Keycloak realm this tenant's people live in.
	Realm string `json:"realm,omitempty"`
	// Apps are the profile names installed into it.
	Apps []string `json:"apps"`
	// Protected is true for a tenant the director refuses to retire.
	Protected bool `json:"protected"`
}

// platformTenant is the one tenant that cannot be retired here.
//
// Its realm is the kernel realm, which every administrator signs in against,
// so retiring it is not a tenant going away but the cluster locking everyone
// out at once. Its manifest says deletionPolicy: Retain for the same reason;
// this is the second lock, on the path a person can reach through a UI.
const platformTenant = "platform"

// TenantDetails lists this cluster's tenants with what the manifests say.
func (g *GitOps) TenantDetails(ctx context.Context) ([]Tenant, error) {
	names, err := g.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Tenant, 0, len(names))
	for _, name := range names {
		t := Tenant{Name: name, Apps: []string{}, Protected: name == platformTenant}
		file, err := g.TenantFile(ctx, name)
		if err == nil {
			if b, readErr := os.ReadFile(file); readErr == nil {
				var doc struct {
					Spec struct {
						DisplayName string `json:"displayName"`
						Isolation   struct {
							KeycloakRealm string `json:"keycloakRealm"`
						} `json:"isolation"`
						Apps []struct {
							Profile string `json:"profile"`
						} `json:"apps"`
					} `json:"spec"`
				}
				if yamlErr := yaml.Unmarshal(b, &doc); yamlErr == nil {
					t.DisplayName = doc.Spec.DisplayName
					t.Realm = doc.Spec.Isolation.KeycloakRealm
					for _, a := range doc.Spec.Apps {
						if a.Profile != "" {
							t.Apps = append(t.Apps, a.Profile)
						}
					}
				}
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// NewTenant is what a caller asks for.
type NewTenant struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// CreateTenant writes a tenant's manifest and commits it.
//
// One file, with the defaults every tenant starts on. Deliberately not a form
// with twenty fields: the things that differ between tenants early on are the
// name and what it is called, and everything else is a plan the cluster
// already has an opinion about. A tenant that needs different quotas gets them
// by editing the manifest afterwards, which is a second reviewable commit
// rather than a twenty-field screen nobody fills in correctly the first time.
func (g *GitOps) CreateTenant(ctx context.Context, req NewTenant, meta Meta) (Result, error) {
	if !ValidName(req.Name) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, req.Name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	dir := filepath.Join(g.path, "clusters", cluster, "tenants", req.Name)
	file := filepath.Join(dir, "tenant.yaml")
	if _, err := os.Stat(file); err == nil {
		return Result{}, fmt.Errorf("%w: %q", ErrTenantExists, req.Name)
	}
	display := strings.TrimSpace(req.DisplayName)
	if display == "" {
		display = req.Name
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(file, []byte(tenantManifest(req.Name, display)), 0o644); err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(g.path, file)
	if err != nil {
		return Result{}, err
	}
	if err := g.commitPaths(ctx, []string{rel}, fmt.Sprintf("Add tenant %s", req.Name), meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "created")
}

// RetireTenant removes a tenant's directory and commits the removal.
//
// Removal and not a flag, because Argo CD prunes what git stops describing and
// the operator's teardown follows from the Tenant object going. A flag would
// mean two sources of truth about whether a tenant exists.
//
// What this does NOT do is decide whether the data goes. The manifest's
// deletionPolicy governs that, the operator honours it, and a tenant created
// with Retain keeps its database and its bucket after this commit. Saying so
// here because "retire" reads like it might mean "delete everything", and the
// console has to be able to tell a person which it was.
func (g *GitOps) RetireTenant(ctx context.Context, tenant string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if tenant == platformTenant {
		return Result{}, fmt.Errorf("%w: %q carries the kernel realm every administrator signs in against", ErrTenantProtected, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	dir := filepath.Join(g.path, "clusters", cluster, "tenants", tenant)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("%w: %q", ErrTenantNotFound, tenant)
	}
	if err := os.RemoveAll(dir); err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(g.path, dir)
	if err != nil {
		return Result{}, err
	}
	if err := g.commitPaths(ctx, []string{rel}, fmt.Sprintf("Retire tenant %s", tenant), meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "retired")
}

// tenantManifest is the file a new tenant starts as.
//
// Written as text rather than marshalled from a struct so the comments survive
// into the repository. Whoever reads this file next is reading a commit in a
// review, and a manifest that explains its own defaults is worth more there
// than one that is merely valid.
func tenantManifest(name, display string) string {
	return fmt.Sprintf(`# Tenant %s, brought on through the director.
#
# Argo CD syncs this file and the operator does the rest: the Keycloak realm,
# the namespaces, the database, and the desktop at console.<kernel>. Nothing
# about the tenant exists until this file does, and editing it is how it
# changes.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: %s
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  displayName: %s
  isolation:
    # A namespace per tenant and a realm of its own. The realm is what keeps
    # one tenant's administrators from seeing another's people at all, rather
    # than being trusted not to look.
    mode: namespace
    keycloakRealm: %s
    databasePrefix: %s_
    s3Prefix: %s-
  # Retain, so retiring the tenant does not take its data with it. Changing
  # this to Delete is a deliberate, reviewable edit.
  deletionPolicy: Retain
  # The base plan's capacity, which is what every tenant starts on. A tenant
  # that needs more gets it by editing this block, which is one more commit.
  quotas:
    requestsCpu: "4"
    requestsMemory: 16Gi
    cpu: "16"
    memory: 32Gi
    storage: 50Gi
    maxApps: 20
  # Apps are installed through the director, which appends to this list.
  apps: []
`, name, name, display, name, name, name)
}
