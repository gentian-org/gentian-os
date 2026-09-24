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

	"sigs.k8s.io/yaml"
)

// App is one entry of a tenant's apps list as git records it.
type App struct {
	Profile string   `json:"profile"`
	Addons  []string `json:"addons,omitempty"`
}

// Apps returns what git says a tenant has installed. Git is the desired state;
// what the cluster has made of it is the operator's to report, not the
// director's.
func (g *GitOps) Apps(ctx context.Context, tenant string) ([]App, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Apps []App `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if doc.Spec.Apps == nil {
		return []App{}, nil
	}
	return doc.Spec.Apps, nil
}

// Tenants lists the tenants deployed on this cluster: every directory under
// clusters/<cluster>/tenants/ that holds a tenant.yaml, by the name in the
// file. Git is the list of record; what the cluster has made of it is the
// operator's to report.
func (g *GitOps) Tenants(ctx context.Context) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	entries, err := os.ReadDir(filepath.Join(g.path, "clusters", cluster, "tenants"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(g.path, "clusters", cluster, "tenants", e.Name(), "tenant.yaml"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if doc.Metadata.Name == "" || !ValidName(doc.Metadata.Name) {
			continue
		}
		out = append(out, doc.Metadata.Name)
	}
	return out, nil
}

// ErrNoClusterClaim is returned when the repository has no Cluster claim for
// this cluster.
var ErrNoClusterClaim = errors.New("no Cluster claim in the repository")

// PlatformRoles returns the Keycloak group that holds each platform role,
// keyed by the cluster relation it grants, from the cluster's Cluster claim.
//
// The claim in git is where authority over a cluster is written down, so this
// is read from the same file the kernel domain comes from rather than from the
// cluster: a cluster cannot widen its own administrators' rights by editing
// something inside itself.
func (g *GitOps) PlatformRoles(ctx context.Context) (map[string]string, error) {
	var claim struct {
		Spec struct {
			PlatformRoles struct {
				Admin           string `json:"admin"`
				SecurityOfficer string `json:"securityOfficer"`
				Auditor         string `json:"auditor"`
				ServiceAdmin    string `json:"serviceAdmin"`
				SharedAppsAdmin string `json:"sharedAppsAdmin"`
				BreakGlass      string `json:"breakGlass"`
			} `json:"platformRoles"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return nil, err
	}
	r := claim.Spec.PlatformRoles
	// The claim's field names are what an operator writes; the map's keys are
	// the model's relations. Translating here keeps the model's vocabulary out
	// of the claim and the claim's spelling out of the model.
	out := map[string]string{}
	for rel, group := range map[string]string{
		"admin":             r.Admin,
		"security_officer":  r.SecurityOfficer,
		"auditor":           r.Auditor,
		"service_admin":     r.ServiceAdmin,
		"shared_apps_admin": r.SharedAppsAdmin,
		"break_glass":       r.BreakGlass,
	} {
		if group != "" {
			out[rel] = group
		}
	}
	return out, nil
}

// KernelDomain returns the kernel domain the cluster's Cluster claim declares
// (clusters/<cluster>/kernel/claims/cluster.yaml). The claim in git is the one
// source of that name; the director does not ask the cluster.
// readClusterClaim parses this cluster's Cluster claim into out. One reader,
// so every field taken from the claim comes from the same file and the same
// "which cluster am I" answer.
func (g *GitOps) readClusterClaim(ctx context.Context, out any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	raw, err := os.ReadFile(filepath.Join(g.path, "clusters", cluster, "kernel", "claims", "cluster.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoClusterClaim
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse cluster claim: %w", err)
	}
	return nil
}

func (g *GitOps) KernelDomain(ctx context.Context) (string, error) {
	var claim struct {
		Spec struct {
			KernelDomain string `json:"kernelDomain"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return "", err
	}
	if claim.Spec.KernelDomain == "" {
		return "", fmt.Errorf("%w: the claim declares no kernelDomain", ErrNoClusterClaim)
	}
	return claim.Spec.KernelDomain, nil
}
