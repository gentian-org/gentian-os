/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/gentian-org/gentian-os/internal/layout"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Repositories are where a tenant or the cluster installs software from: a
// tenant's own private app repository alongside the cluster's.
//
// The custodian does not declare them. Where software comes from is
// configuration -- it decides what may enter -- so the address is a commit
// the director makes, with an author, and Argo CD applies it. This process
// once created, changed and deleted these objects in the cluster itself,
// which left "who pointed this tenant at that address" in no commit at all
// and made the keeper of the keys a second place configuration was decided.
//
// What is left here is what belongs to a credential. The list below says
// which repositories exist and which credential belongs to each, read from
// the objects Argo CD applied. Setting the password is the credentials route
// (PUT /v1/credentials/repository-<name>): it works from the requirement the
// composition emits for a repository that is declared, takes its vault path
// from there, and has nothing to act on for one that is not.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=repositories,verbs=get;list;watch

var repositoryGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "Repository",
}

// repositoryNamespace holds Repository claims. They are namespaced because
// claims are, not because a tenant owns a namespace here.
var repositoryNamespace = layout.Namespace(layout.Provisioning)

// roleApps is what a repository that states no role is listed as.
const roleApps = "apps"

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
	// repository everyone's apps come from. Changing one is asked of the
	// director.
	Owned bool `json:"owned"`

	// CredentialName ties this to the requirement that supplies its credential,
	// so the console can link the two instead of making the operator match
	// names by eye.
	CredentialName string `json:"credentialName,omitempty"`
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
		if tenant != "" && !v.canSee(scopeTenant, tenant) {
			continue
		}
		// The cluster's own are shown to whoever may read the cluster's
		// credentials, and to a tenant's administrator, for whom they are
		// what the tenant installs from.
		if tenant == "" && !v.canSee(scopeCluster, "") && v.Tenant == "" {
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
		Owned:          v.canWriteRepository(tenant),
		CredentialName: "repository-" + item.GetName(),
	}
}

// canWriteRepository reports whether a repository is the caller's own: the
// cluster's, for whoever may set the cluster's credentials; a tenant's, for
// whoever may set that tenant's. It marks the listing and decides nothing
// else here. The director asks the store the same relation on the same object
// before it commits a change to one.
func (v Viewer) canWriteRepository(tenant string) bool {
	if tenant == "" {
		return v.canWrite(scopeCluster, "")
	}
	return v.canWrite(scopeTenant, tenant)
}
