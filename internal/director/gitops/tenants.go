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
	"time"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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

// ErrSingleTenancy is a new tenant on a cluster whose tenancy mode is single:
// its one tenant is the platform tenant, which the install already made.
var ErrSingleTenancy = errors.New("this cluster is single-tenant; its one tenant is the platform tenant")

// refuseInSingleTenancy answers ErrSingleTenancy on a single-tenant cluster,
// before anything is committed: the operator would refuse the tenant too,
// but only once git already held it.
func (g *GitOps) refuseInSingleTenancy(ctx context.Context) error {
	settings, err := g.ClusterSettingValues(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return err
	}
	if gentianov1alpha1.NormalizeTenancyMode(settings["tenancyMode"]) == gentianov1alpha1.TenancyModeSingle {
		return ErrSingleTenancy
	}
	return nil
}

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
	// LoginDomain is the domain its people sign in under, and so where the
	// sign-in router sends an address on it: see TenantLoginDomain.
	LoginDomain string `json:"loginDomain,omitempty"`
	// Protected is true for a tenant the director refuses to retire.
	Protected bool `json:"protected"`
	// Purging is true once a purge was asked for: the manifest says
	// deletionPolicy: Delete and carries PurgeAnnotation, and the director
	// removes it as soon as the cluster has taken that in.
	Purging bool `json:"purging,omitempty"`
}

// PurgeAnnotation marks a tenant whose purge was asked for and not finished.
// Its value is when.
const PurgeAnnotation = "gentianos.io/purge-requested"

// platformTenant is the one tenant that cannot be retired here.
//
// Its realm is the kernel realm, which every administrator signs in against,
// so retiring it is not a tenant going away but the cluster locking everyone
// out at once. Its manifest says deletionPolicy: Retain for the same reason;
// this is the second lock, on the path a person can reach through a UI.
const platformTenant = "platform"

// PlatformTenant is that tenant's name, for a caller outside this package.
const PlatformTenant = platformTenant

// TenantRealm is the Keycloak realm one tenant's people live in.
//
// Read from the manifest, and defaulted to the tenant's own name exactly as
// the operator defaults it. The two must agree: the operator provisions the
// director's credential per realm, and a director that resolved a different
// realm name would ask for a credential nobody wrote -- which reads as "this
// tenant has no identity" rather than as the disagreement it is.
func (g *GitOps) TenantRealm(ctx context.Context, tenant string) (string, error) {
	if !ValidName(tenant) {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := g.TenantFile(ctx, tenant)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var doc struct {
		Spec struct {
			Isolation struct {
				KeycloakRealm string `json:"keycloakRealm"`
			} `json:"isolation"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	if realm := doc.Spec.Isolation.KeycloakRealm; realm != "" {
		return realm, nil
	}
	return tenant, nil
}

// TenantLoginDomain is the domain a tenant's people sign in under: the part
// after the @ in a login the console composes from a local part.
//
// The custom domain when a TenantDomain beside its manifest binds one. The kernel domain for
// a tenant that adopts another realm -- the platform tenant, whose people are
// the kernel realm's administrators (admin@<kernel>, not
// admin@platform.<kernel>) -- and under single tenancy, where the one tenant
// is the cluster. Otherwise <tenant>.<kernel>, the operator's default.
func (g *GitOps) TenantLoginDomain(ctx context.Context, tenant string) (string, error) {
	if !ValidName(tenant) {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := g.TenantFile(ctx, tenant)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var doc struct {
		Spec struct {
			Isolation struct {
				KeycloakRealm string `json:"keycloakRealm"`
			} `json:"isolation"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	kernel, err := g.KernelDomain(ctx)
	if err != nil {
		return "", err
	}
	custom, err := tenantCustomDomain(file)
	if err != nil {
		return "", err
	}
	// The operator refuses a custom domain on the kernel domain and leaves
	// the tenant where it was; so does this, or the two would disagree.
	if custom != "" && custom != kernel && !strings.HasSuffix(custom, "."+kernel) {
		return custom, nil
	}
	if realm := doc.Spec.Isolation.KeycloakRealm; realm != "" && realm != tenant {
		return kernel, nil
	}
	settings, err := g.ClusterSettingValues(ctx)
	if err == nil && settings["tenancyMode"] == "single" {
		return kernel, nil
	}
	return tenant + "." + kernel, nil
}

// TenantDomainFile is where an extension that gives tenants custom domains
// commits a tenant's TenantDomain: beside its tenant.yaml, so Argo CD applies
// it with the tenant and the director reads it from the same tree. The OS
// itself never writes one.
const TenantDomainFile = "domain.yaml"

// tenantCustomDomain is the domain the TenantDomain beside tenantFile binds,
// or "" when there is none.
func tenantCustomDomain(tenantFile string) (string, error) {
	file := filepath.Join(filepath.Dir(tenantFile), TenantDomainFile)
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var doc struct {
		Kind string `json:"kind"`
		Spec struct {
			Domain string `json:"domain"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	if doc.Kind != "TenantDomain" {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(doc.Spec.Domain)), nil
}

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
					Metadata struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
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
					t.Purging = doc.Metadata.Annotations[PurgeAnnotation] != ""
					t.Realm = doc.Spec.Isolation.KeycloakRealm
					for _, a := range doc.Spec.Apps {
						if a.Profile != "" {
							t.Apps = append(t.Apps, a.Profile)
						}
					}
				}
			}
		}
		if domain, err := g.TenantLoginDomain(ctx, name); err == nil {
			t.LoginDomain = domain
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
	// RequireMFA makes the administrator enrol a second factor when they
	// activate the account. Nil is the default, which is on.
	RequireMFA *bool `json:"requireMFA,omitempty"`
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
	if err := g.refuseInSingleTenancy(ctx); err != nil {
		return Result{}, err
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
	requireMFA := req.RequireMFA == nil || *req.RequireMFA
	if err := os.WriteFile(file, []byte(tenantManifest(req.Name, display, requireMFA)), 0o644); err != nil {
		return Result{}, err
	}
	// The kustomization the bootstrap scaffolds for the platform tenant,
	// written here for every tenant so that the directory is a kustomization
	// from the start rather than becoming one the day a plan is chosen.
	kustomization := filepath.Join(dir, "kustomization.yaml")
	if err := os.WriteFile(kustomization, []byte(tenantKustomization()), 0o644); err != nil {
		return Result{}, err
	}
	rels := make([]string, 0, 2)
	for _, f := range []string{file, kustomization} {
		rel, err := filepath.Rel(g.path, f)
		if err != nil {
			return Result{}, err
		}
		rels = append(rels, rel)
	}
	if err := g.commitPaths(ctx, rels, fmt.Sprintf("Add tenant %s", req.Name), meta); err != nil {
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

// RequestTenantPurge is the first half of purging a tenant: its manifest
// says deletionPolicy: Delete, and carries PurgeAnnotation so the second half
// can be found again after a restart.
//
// Two commits, not one, because Argo CD applies only what git says at the
// moment it syncs. A manifest switched to Delete and removed before that sync
// is pruned as the cluster last saw it -- Retain -- and the data the purge was
// for stays. So the removal waits until the live Tenant carries Delete; the
// caller watches for that and then calls RetireTenant.
// PurgeOptions refine a purge.
type PurgeOptions struct {
	// KeepBundles spares the tenant's backup bucket: the offboarding case,
	// where the tenant was handed a copy and a provider keeps one.
	KeepBundles bool `json:"keepBundles,omitempty"`
}

func (g *GitOps) RequestTenantPurge(ctx context.Context, tenant string, now time.Time, opts PurgeOptions, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if tenant == platformTenant {
		return Result{}, fmt.Errorf("%w: %q carries the kernel realm every administrator signs in against", ErrTenantProtected, tenant)
	}
	return g.apply(ctx, tenant, fmt.Sprintf("Purge tenant %s: delete its data when it is retired", tenant), meta,
		func(text string) (string, string, bool, error) {
			out, policyChanged, err := setKeyPath(text, "spec", []string{"deletionPolicy"}, "Delete")
			if err != nil {
				return "", "", false, err
			}
			if opts.KeepBundles {
				var kept bool
				out, kept, err = setKeyPath(out, "spec", []string{"deletion", "keepBundles"}, "true")
				if err != nil {
					return "", "", false, err
				}
				policyChanged = policyChanged || kept
			}
			marked := tenantPurgeRequested(out)
			if !marked {
				out, _, err = setKeyPath(out, "metadata", []string{"annotations", PurgeAnnotation}, now.UTC().Format(time.RFC3339))
				if err != nil {
					return "", "", false, err
				}
			}
			if !policyChanged && marked {
				return text, "purge_pending", false, nil
			}
			return out, "purge_requested", true, nil
		})
}

// PendingPurges are the tenants whose purge was asked for and not finished.
func (g *GitOps) PendingPurges(ctx context.Context) ([]string, error) {
	tenants, err := g.TenantDetails(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tenants {
		if t.Purging && !t.Protected {
			out = append(out, t.Name)
		}
	}
	return out, nil
}

func tenantPurgeRequested(text string) bool {
	var doc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	return yaml.Unmarshal([]byte(text), &doc) == nil && doc.Metadata.Annotations[PurgeAnnotation] != ""
}

// tenantManifest is the file a new tenant starts as.
//
// Written as text rather than marshalled from a struct so the comments survive
// into the repository. Whoever reads this file next is reading a commit in a
// review, and a manifest that explains its own defaults is worth more there
// than one that is merely valid.
func tenantManifest(name, display string, requireMFA bool) string {
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
  # The administrator account has no password until its holder sets one,
  # through a single-use link the director issues (activate-admin).
  admin:
    requireMFA: %t
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
`, name, name, display, requireMFA, name, name, name)
}

// TenantAdminRequiresMFA is whether the tenant's administrator must enrol a
// second factor: the manifest's spec.admin.requireMFA, true when unset.
func (g *GitOps) TenantAdminRequiresMFA(ctx context.Context, tenant string) (bool, error) {
	if !ValidName(tenant) {
		return true, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := g.TenantFile(ctx, tenant)
	if err != nil {
		return true, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return true, err
	}
	var doc struct {
		Spec struct {
			Admin struct {
				RequireMFA *bool `json:"requireMFA"`
			} `json:"admin"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return true, fmt.Errorf("read %s: %w", file, err)
	}
	return doc.Spec.Admin.RequireMFA == nil || *doc.Spec.Admin.RequireMFA, nil
}
