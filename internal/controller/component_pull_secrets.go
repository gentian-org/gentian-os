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
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The credential a tenant's app is pulled with.
//
// A tenant declares where it installs from as a Repository, and sets that
// repository's password at the custodian. The repository's Composition turns
// the two into one Secret per registry repository, in that tenant's namespace
// and in no other: repository-<name>-pull, holding the credential both as a
// dockerconfigjson, which is what a kubelet reads, and as username and
// password, which is what provider-helm reads.
//
// This file is the other half: which of those Secrets a component's chart and
// pods are told about. Only the names are handled here. Nothing in this
// reconciler reads a pull Secret, and nothing it writes names one outside the
// component's own namespace.
//
// Whose repository one is is read from the Repository's spec.tenant, which
// the director writes from the route the caller was authorised on, and is
// compared with the Tenant the component's namespace belongs to. It is the
// same field the Composition scopes the Secret by, so the two cannot
// disagree about where a credential may go.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=repositories,verbs=get;list;watch

var repositoryClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "Repository",
}

// repositoryClaimNamespace is where Repository claims are applied. A claim
// anywhere else is not one the director declared, and is not read.
var repositoryClaimNamespace = layout.Namespace(layout.Provisioning)

// pullRepository is a registry repository a tenant declared with a credential.
type pullRepository struct {
	name string
	host string
	path []string
}

// secretName is the Secret the repository's Composition makes in the tenant's
// namespace. Derived from the repository's name and nothing else, so it can
// be referred to before it exists.
func (p pullRepository) secretName() string { return "repository-" + p.name + "-pull" }

// chartPull is the Secret one chart repository address is pulled with.
type chartPull struct {
	repository string
	secretName string
	// declared is the name of the Repository the Secret belongs to, for
	// telling a person which credential a failed pull was made with.
	declared string
}

// pullSecrets is what a component's release is told about pull credentials.
type pullSecrets struct {
	// images are the Secrets the component's pods may pull with: every
	// registry repository the tenant declared. Which registry an image comes
	// from is not something a profile states, and a kubelet tries each Secret
	// it is given, so they are all named.
	images []string
	// charts are the profile's chart repositories that one declared
	// repository answers for.
	charts []chartPull
}

func (p pullSecrets) chart(repository string) *chartPull {
	for i := range p.charts {
		if p.charts[i].repository == repository {
			return &p.charts[i]
		}
	}
	return nil
}

// withPullHint adds, to the reason a release is not ready, which of the
// tenant's credentials its chart is pulled with. Said only for a chart that
// is pulled with one: for any other the reason stands as the provider gave it.
func (p pullSecrets) withPullHint(message string, profile *gentianov1alpha1.ComponentProfile) string {
	if message == "" {
		return message
	}
	var declared []string
	for _, repository := range profileChartRepositories(profile) {
		if c := p.chart(repository); c != nil {
			declared = append(declared, c.declared)
		}
	}
	if len(declared) == 0 {
		return message
	}
	return fmt.Sprintf("%s (the chart is pulled with the credential of repository %s; "+
		"if the pull was refused, set that credential again in the credential manager)",
		message, strings.Join(declared, ", "))
}

// registryAddress splits a repository address into its host and the segments
// of its path. The scheme is dropped: a Repository is declared as
// https://registry.example/acme and a chart as oci://registry.example/acme,
// and they are the same place.
func registryAddress(address string) (string, []string) {
	a := strings.TrimSpace(address)
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexAny(a, "?#"); i >= 0 {
		a = a[:i]
	}
	host, rest, _ := strings.Cut(a, "/")
	// Never part of where a registry is, and a declared address has no
	// business carrying a credential.
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	var path []string
	for _, seg := range strings.Split(rest, "/") {
		if seg != "" {
			path = append(path, seg)
		}
	}
	return strings.ToLower(host), path
}

// answersFor reports whether a chart at the given address lies inside this
// repository: the same host, port included, and the repository's path a
// prefix of the chart's by whole segments. registry.example/acme answers for
// registry.example/acme/charts and not for registry.example/acme-other.
func (p pullRepository) answersFor(host string, path []string) bool {
	if p.host == "" || p.host != host || len(p.path) > len(path) {
		return false
	}
	for i, seg := range p.path {
		if path[i] != seg {
			return false
		}
	}
	return true
}

// matchChartRepository finds the declared repositories a chart address lies
// inside. One is the answer; none is a public chart; more than one is a
// question this does not answer for the tenant.
func matchChartRepository(chartRepository string, repos []pullRepository) []pullRepository {
	host, path := registryAddress(chartRepository)
	var out []pullRepository
	for _, repo := range repos {
		if repo.answersFor(host, path) {
			out = append(out, repo)
		}
	}
	return out
}

// tenantPullRepositories lists the registry repositories a tenant declared
// with a credential a pull Secret is made from.
//
// The tenant is matched exactly. The cluster's own repositories have no
// tenant and are never returned: what the cluster pulls with reaches a
// tenant's namespace as registry-credentials and by no other way.
func (r *ComponentReconciler) tenantPullRepositories(ctx context.Context, tenant string) ([]pullRepository, error) {
	if tenant == "" {
		return nil, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(repositoryClaimGVK.GroupVersion().WithKind(repositoryClaimGVK.Kind + "List"))
	if err := r.List(ctx, list, client.InNamespace(repositoryClaimNamespace)); err != nil {
		// The kind is defined by the repository XRD. A cluster on which that
		// is not established yet has no repositories.
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	var out []pullRepository
	for i := range list.Items {
		spec, _, _ := unstructured.NestedMap(list.Items[i].Object, "spec")
		if owner, _ := spec["tenant"].(string); owner != tenant {
			continue
		}
		if kind, _ := spec["type"].(string); kind != "oci" {
			continue
		}
		// The same conditions the Composition makes the Secret under. A
		// repository without a credential is public; one with a bearer token
		// has no username, and neither a kubelet nor Helm pulls without one.
		cred, _ := spec["credential"].(map[string]interface{})
		if len(cred) == 0 {
			continue
		}
		if auth, _ := cred["authType"].(string); auth == "bearer" {
			continue
		}
		address, _, _ := unstructured.NestedString(spec, "endpoints", "inCluster")
		host, path := registryAddress(address)
		if host == "" {
			continue
		}
		out = append(out, pullRepository{name: list.Items[i].GetName(), host: host, path: path})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out, nil
}

// profileChartRepositories are the addresses a profile's charts are pulled
// from: its own and each extension's.
func profileChartRepositories(profile *gentianov1alpha1.ComponentProfile) []string {
	var out []string
	seen := map[string]bool{}
	add := func(repository string) {
		if repository != "" && !seen[repository] {
			seen[repository] = true
			out = append(out, repository)
		}
	}
	if chart := profile.Spec.Package.Chart; chart != nil {
		add(chart.Repository)
	}
	for i := range profile.Spec.Extensions {
		add(profile.Spec.Extensions[i].Chart.Repository)
	}
	return out
}

// resolvePullSecrets works out what a profile's charts and pods are told
// about the tenant's repositories. A message is a refusal a tenant
// administrator can act on, and nothing is rolled out while there is one.
func resolvePullSecrets(profile *gentianov1alpha1.ComponentProfile, repos []pullRepository) (pullSecrets, string) {
	var out pullSecrets
	for _, repo := range repos {
		out.images = append(out.images, repo.secretName())
	}
	for _, repository := range profileChartRepositories(profile) {
		matches := matchChartRepository(repository, repos)
		switch len(matches) {
		case 0:
		case 1:
			out.charts = append(out.charts, chartPull{repository: repository, secretName: matches[0].secretName(), declared: matches[0].name})
		default:
			names := make([]string, 0, len(matches))
			for _, m := range matches {
				names = append(names, m.name)
			}
			// Not the longest match and not the first: choosing for the
			// tenant which of its credentials is sent where is a guess, and
			// a wrong one presents a password to a path it was not set for.
			return pullSecrets{}, fmt.Sprintf(
				"the chart at %s lies inside more than one repository this tenant declared (%s); "+
					"which credential it is pulled with is not guessed. Remove or narrow one of them",
				repository, strings.Join(names, ", "))
		}
	}
	return out, ""
}

// valuesWithPullSecrets names the tenant's pull Secrets in a chart's values,
// the way the app Composition names the cluster's: under imagePullSecrets and
// global.imagePullSecrets, after whatever the profile already lists there or
// registry-credentials when it lists nothing. A tenant that declared no
// registry repository leaves the values exactly as they were.
func valuesWithPullSecrets(values map[string]interface{}, profile *gentianov1alpha1.ComponentProfile, images []string) {
	if len(images) == 0 {
		return
	}
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if listed, ok := values["imagePullSecrets"].([]interface{}); ok {
		for _, entry := range listed {
			switch e := entry.(type) {
			case string:
				add(e)
			case map[string]interface{}:
				name, _ := e["name"].(string)
				add(name)
			}
		}
	}
	if len(names) == 0 {
		add(defaultPullSecretName)
	}
	for _, name := range images {
		add(name)
	}
	// Charts disagree about the shape, and the profile says which its chart
	// takes -- the same annotation the app Composition reads.
	standard := profile.Annotations[standardPullSecretsAnnotation] == "true"
	rendered := make([]interface{}, 0, len(names))
	for _, name := range names {
		if standard {
			rendered = append(rendered, map[string]interface{}{"name": name})
		} else {
			rendered = append(rendered, name)
		}
	}
	values["imagePullSecrets"] = rendered
	global, _ := values["global"].(map[string]interface{})
	if global == nil {
		global = map[string]interface{}{}
	}
	global["imagePullSecrets"] = append([]interface{}{}, rendered...)
	values["global"] = global
}

const (
	// defaultPullSecretName is the cluster's own pull credential, replicated
	// into every tenant namespace.
	defaultPullSecretName = "registry-credentials"
	// standardPullSecretsAnnotation marks a profile whose chart takes
	// imagePullSecrets as a list of {name}, rather than of names.
	standardPullSecretsAnnotation = "gentianos.io/standard-image-pull-secrets"
)

// appClaimPullSecrets is pullSecrets as the App claim carries it.
func appClaimPullSecrets(p pullSecrets) map[string]interface{} {
	out := map[string]interface{}{}
	if len(p.images) > 0 {
		images := make([]interface{}, 0, len(p.images))
		for _, name := range p.images {
			images = append(images, name)
		}
		out["images"] = images
	}
	if len(p.charts) > 0 {
		charts := make([]interface{}, 0, len(p.charts))
		for _, c := range p.charts {
			charts = append(charts, map[string]interface{}{"repository": c.repository, "secretName": c.secretName})
		}
		out["charts"] = charts
	}
	return out
}

// composedReleaseMessage says why the release the app Composition made for a
// component is not ready, in the provider's own words: a chart that could not
// be pulled says so there and nowhere a tenant administrator would look.
// Empty when there is nothing to say yet.
func (r *ComponentReconciler) composedReleaseMessage(ctx context.Context, claim *unstructured.Unstructured) string {
	composite, _, _ := unstructured.NestedString(claim.Object, "spec", "resourceRef", "name")
	if composite == "" {
		return ""
	}
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: composite + "-release"}, release); err != nil {
		return ""
	}
	if crossplaneObjectReady(release) {
		return ""
	}
	return releaseFailureOf(release)
}
