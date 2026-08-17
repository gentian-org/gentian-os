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

package credentialmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Repository claims are the tenant-facing half of app distribution: a tenant
// adds its own private app repository alongside the cluster's, or points its
// deployments at somewhere else entirely.
//
// This lives beside the credential manager rather than in the app-lifecycle API
// because it needs an identity. That API takes its tenant from the request path
// and its actor from a header, which is safe only while every caller is already
// trusted. Declaring a repository is not that: replacing a tenant's deployments
// repository redirects everything Argo CD reconciles for them, and "which tenant
// is asking" therefore has to be established rather than stated.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=repositories,verbs=get;list;watch;create;update;patch;delete

var repositoryGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "Repository",
}

// repositoryNamespace holds Repository claims. They are namespaced because
// claims are, not because a tenant owns a namespace here.
const repositoryNamespace = "crossplane-system"

// RepositoryRole distinguishes what losing a repository costs.
//
// `apps` is additive: a tenant's private catalogue sits alongside the cluster's
// and removing it removes those apps. `deployments` is the tenant's source of
// truth — repointing it changes what every one of their apps reconciles from,
// which is why it is treated as destructive even when the object is new.
const (
	roleApps        = "apps"
	roleDeployments = "deployments"
)

// RepositoryView is what the API says about one repository. As everywhere in
// this package there is no field capable of carrying a credential value — the
// credential is a separate CredentialRequirement, supplied through the same API
// and never read back.
type RepositoryView struct {
	Name     string `json:"name"`
	Tenant   string `json:"tenant,omitempty"`
	Role     string `json:"role"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Branch   string `json:"branch,omitempty"`
	Writable bool   `json:"writable"`

	// Owned is false for the cluster's own repositories, which a tenant admin
	// can see but not change. The console greys those rather than hiding them:
	// "you cannot edit this" is more useful than a list that omits the base
	// repository everyone's apps come from.
	Owned bool `json:"owned"`

	// CredentialName ties this to the requirement that supplies its credential,
	// so the console can link the two instead of making the operator match
	// names by eye.
	CredentialName string `json:"credentialName,omitempty"`
}

// repositoryRequest is the write body.
type repositoryRequest struct {
	Role     string `json:"role"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Branch   string `json:"branch,omitempty"`
	Writable bool   `json:"writable,omitempty"`

	// Confirm must repeat the repository name for any change that is not purely
	// additive. It is the API half of the console's danger zone: a confirmation
	// enforced only in the UI is a confirmation a script skips.
	Confirm string `json:"confirm,omitempty"`
}

func (s *Server) handleListRepositories(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	items, err := s.listRepositories(r.Context(), c.view)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": items})
}

func (s *Server) listRepositories(ctx context.Context, v Viewer) ([]RepositoryView, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(repositoryGVK.GroupVersion().WithKind(repositoryGVK.Kind + "List"))
	if err := s.Catalogue.Client.List(ctx, &list, client.InNamespace(repositoryNamespace)); err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}

	out := make([]RepositoryView, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		tenant, _, _ := unstructured.NestedString(item.Object, "spec", "tenant")

		// A tenant sees its own and the cluster's; the cluster's are read-only
		// for them. Another tenant's are not shown at all.
		if !v.ClusterAdmin && tenant != "" && tenant != v.Tenant {
			continue
		}
		out = append(out, s.repositoryView(item, tenant, v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) repositoryView(item *unstructured.Unstructured, tenant string, v Viewer) RepositoryView {
	url, _, _ := unstructured.NestedString(item.Object, "spec", "endpoints", "external")
	if url == "" {
		url, _, _ = unstructured.NestedString(item.Object, "spec", "endpoints", "inCluster")
	}
	typ, _, _ := unstructured.NestedString(item.Object, "spec", "type")
	branch, _, _ := unstructured.NestedString(item.Object, "spec", "branch")
	writable, _, _ := unstructured.NestedBool(item.Object, "spec", "writable")
	role, _, _ := unstructured.NestedString(item.Object, "spec", "role")
	if role == "" {
		role = roleApps
	}
	return RepositoryView{
		Name:           item.GetName(),
		Tenant:         tenant,
		Role:           role,
		Type:           typ,
		URL:            url,
		Branch:         branch,
		Writable:       writable,
		Owned:          v.ClusterAdmin || (tenant != "" && tenant == v.Tenant),
		CredentialName: "repository-" + item.GetName(),
	}
}

func (s *Server) handleSetRepository(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	name := r.PathValue("name")

	var body repositoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed body: %w", err))
		return
	}
	if err := checkRepositoryRequest(name, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(repositoryGVK)
	getErr := s.Catalogue.Client.Get(r.Context(),
		client.ObjectKey{Namespace: repositoryNamespace, Name: name}, existing)
	found := getErr == nil
	if getErr != nil && !errors.IsNotFound(getErr) {
		writeErr(w, http.StatusInternalServerError, getErr)
		return
	}

	owner := c.view.Tenant
	if found {
		existingTenant, _, _ := unstructured.NestedString(existing.Object, "spec", "tenant")
		if !c.view.canWriteRepository(existingTenant) {
			// 404 rather than 403: a tenant learning that a name is taken by
			// another tenant is itself a disclosure.
			writeErr(w, http.StatusNotFound, fmt.Errorf("no such repository: %s", name))
			return
		}
		owner = existingTenant
	} else if c.view.ClusterAdmin && c.view.Tenant == "" {
		// A cluster admin with no tenant creates cluster-owned repositories.
		owner = ""
	}

	if reason := repositoryNeedsConfirmation(existing, found, &body); reason != "" {
		if body.Confirm != name {
			writeJSON(w, http.StatusPreconditionRequired, map[string]any{
				"error":          reason,
				"confirmField":   "confirm",
				"confirmWith":    name,
				"dangerous":      true,
				"requiresRetype": true,
			})
			return
		}
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(repositoryGVK)
	obj.SetNamespace(repositoryNamespace)
	obj.SetName(name)
	spec := map[string]any{
		"type":      body.Type,
		"role":      body.Role,
		"endpoints": map[string]any{"inCluster": body.URL},
		"writable":  body.Writable,
	}
	if body.Branch != "" {
		spec["branch"] = body.Branch
	}
	if owner != "" {
		spec["tenant"] = owner
	}
	// The credential is declared by the composition, not here: it emits a
	// CredentialRequirement whose scope follows spec.tenant. Supplying the value
	// is a separate call to the same API, which is what keeps the value out of
	// this request body.
	spec["credential"] = map[string]any{
		"displayName": fmt.Sprintf("Credentials for %s", name),
		"vaultPath":   repositoryVaultPath(owner, name),
	}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if found {
		existing.Object["spec"] = spec
		if err := s.Catalogue.Client.Update(r.Context(), existing); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	} else if err := s.Catalogue.Client.Create(r.Context(), obj); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":           name,
		"tenant":         owner,
		"role":           body.Role,
		"credentialName": "repository-" + name,
		"created":        !found,
	})
}

func (s *Server) handleDeleteRepository(w http.ResponseWriter, r *http.Request) {
	c, err := s.identify(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	name := r.PathValue("name")

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(repositoryGVK)
	if err := s.Catalogue.Client.Get(r.Context(),
		client.ObjectKey{Namespace: repositoryNamespace, Name: name}, obj); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such repository: %s", name))
		return
	}
	tenant, _, _ := unstructured.NestedString(obj.Object, "spec", "tenant")
	if !c.view.canWriteRepository(tenant) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such repository: %s", name))
		return
	}

	// Removal is always destructive — the apps it carried stop reconciling — so
	// it always retypes, with no additive case to exempt.
	if r.URL.Query().Get("confirm") != name {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error":          fmt.Sprintf("removing %q stops every app it provides from reconciling", name),
			"confirmField":   "confirm",
			"confirmWith":    name,
			"dangerous":      true,
			"requiresRetype": true,
		})
		return
	}
	if err := s.Catalogue.Client.Delete(r.Context(), obj); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}

// canWriteRepository — a tenant admin may change its own tenant's repositories
// and nothing else. A cluster admin may change any.
func (v Viewer) canWriteRepository(tenant string) bool {
	if v.ClusterAdmin {
		return true
	}
	return tenant != "" && tenant == v.Tenant
}

// repositoryNeedsConfirmation returns why an operation is dangerous, or "".
//
// Adding an apps repository is additive and needs no ceremony. Everything else
// changes where something already running reconciles from, and a confirmation
// that is easy to click through is not a confirmation.
func repositoryNeedsConfirmation(existing *unstructured.Unstructured, found bool, body *repositoryRequest) string {
	if body.Role == roleDeployments {
		// Even when new: pointing a tenant's deployments somewhere else
		// redirects everything Argo CD reconciles for them.
		return "this repository is the source of truth for the tenant's deployments; " +
			"changing it redirects everything reconciled from it"
	}
	if !found {
		return ""
	}
	oldURL, _, _ := unstructured.NestedString(existing.Object, "spec", "endpoints", "inCluster")
	if oldURL != "" && oldURL != body.URL {
		return fmt.Sprintf("this replaces the existing source %q, and apps installed from it "+
			"will resolve against the new one", oldURL)
	}
	return ""
}

// repositoryVaultPath keeps a tenant's repository credential inside the prefix
// its OpenBao policy can write and ESO can read. Deriving it here rather than
// accepting one from the caller is deliberate: a path outside that prefix
// produces a requirement the tenant can see and cannot satisfy.
func repositoryVaultPath(tenant, name string) string {
	if tenant == "" {
		return "gentian-os/kernel/repositories/" + name
	}
	return fmt.Sprintf("gentian-os/tenants/%s/repositories/%s", tenant, name)
}

func checkRepositoryRequest(name string, body *repositoryRequest) error {
	if name == "" {
		return fmt.Errorf("a repository name is required")
	}
	if strings.TrimSpace(body.URL) != body.URL || body.URL == "" {
		return fmt.Errorf("url is required and must not have leading or trailing whitespace")
	}
	switch body.Type {
	case "git", "oci":
	default:
		return fmt.Errorf("type must be git or oci, got %q", body.Type)
	}
	switch body.Role {
	case roleApps, roleDeployments:
	default:
		return fmt.Errorf("role must be %s or %s, got %q", roleApps, roleDeployments, body.Role)
	}
	if body.Role == roleDeployments && body.Type != "git" {
		return fmt.Errorf("a deployments repository must be git")
	}
	return nil
}
