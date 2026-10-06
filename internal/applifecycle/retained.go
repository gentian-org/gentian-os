/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// What uninstalled apps still hold.
//
// Uninstalling an app keeps its data, and nothing else on the cluster says
// so afterwards: the app is gone from the tenant's manifest and from the
// list of what is installed, and its database, files, credentials and access
// group sit there unlisted until somebody purges them or installs the app
// again. This read is that list. The state it reports is "retained": an app
// that is uninstalled, data retained.
//
// It is built on the names and the matching a purge uses -- the same
// database record, the same volume-claim rule, the same vault paths, the
// same group name -- so that what it reports for an app is what a purge of
// that app would destroy.
//
// It reads, and only reads: objects from the API server, names (never
// values) from the vault, group names from the identity provider. A kind
// that could only be examined by running something is reported as unknown
// rather than examined. That is the object store and the cache, whose
// contents can be asked only of the store itself, from a Job with its admin
// credential; and a MariaDB database, for the same reason.

// How one kind of data stands for one app.
const (
	RetainedPresent = "present"
	RetainedAbsent  = "absent"
	RetainedUnknown = "unknown"
)

// AppStateRetained is the state of an app this read lists.
const AppStateRetained = "retained"

// RetainedApp is one uninstalled app that still holds data.
type RetainedApp struct {
	Profile string `json:"profile"`
	// State is always "retained".
	State string `json:"state"`
	// ProfileAvailable is whether the app's ComponentProfile is still on the
	// cluster. Without it nothing says which stores the app had: the kinds
	// that depend on that are unknown here, and a purge of the app answers
	// "partially-purged" with the kinds it did not examine.
	ProfileAvailable bool `json:"profileAvailable"`
	// Kinds says, per kind of data, whether it is present, absent or
	// unknown. Every kind is always listed.
	Kinds map[string]string `json:"kinds"`
	// Volumes are the volume claims counted as the app's files.
	Volumes []string `json:"volumes,omitempty"`
}

// RetainedApps is the answer of the read.
type RetainedApps struct {
	Tenant string        `json:"tenant"`
	Apps   []RetainedApp `json:"apps"`
	// Unknown says, per kind, why this read reports it as unknown: for some
	// apps by design (see above), or for all of them because the place that
	// would say could not be asked this time.
	Unknown map[string]string `json:"unknown,omitempty"`
}

const (
	unknownObjectStorage = "whether a bucket exists can only be asked of the object store, from a Job holding its admin credential; this read runs nothing. Unknown for an app whose profile declares object storage, or whose profile is gone"
	unknownCache         = "whether a cache user exists can only be asked of the cache, from a Job holding its admin credential; this read runs nothing. Unknown for an app whose profile declares a cache, or whose profile is gone. The keys an app wrote are not attributable to it at all"
	unknownMariaDB       = "a PostgreSQL database is recorded on the cluster and reported; a MariaDB database is not recorded and could only be asked of the server, so it is unknown for an app whose profile declares one"
)

// retainedSources is what the cluster holds for a tenant, by the name it is
// held under, before any of it is attributed to an app.
type retainedSources struct {
	// databases are the apps a PostgreSQL database is recorded for.
	databases map[string]bool
	// credentials are the keys below the tenant's apps path in the vault: an
	// app's name, or "{app}-{extension}".
	credentials map[string]bool
	// groups are the names an access group exists for: an app's, or
	// "{app}-{extension}".
	groups map[string]bool
	// claims are the volume claims in the tenant's namespace.
	claims []corev1.PersistentVolumeClaim
	// failed says which of the above could not be read.
	failed map[string]string
}

// RetainedApps lists the tenant's uninstalled apps that still hold data.
func (s *Service) RetainedApps(ctx context.Context, tenantName string) (*RetainedApps, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	ns := layout.Tenant(tenantName)

	// What is installed, or still being taken down: none of that is
	// "retained", whatever it holds.
	installed := map[string]bool{}
	for _, a := range tenant.Spec.Apps {
		installed[a.Profile] = true
		for _, addon := range a.Addons {
			installed[addon] = true
		}
	}
	var components gentianov1alpha1.ComponentList
	if err := s.client.List(ctx, &components, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list components: %w", err)
	}
	for i := range components.Items {
		installed[components.Items[i].Name] = true
		installed[components.Items[i].Spec.ProfileRef.Name] = true
	}
	releases, err := s.helmReleases(ctx, ns)
	if err != nil {
		return nil, err
	}

	src := s.retainedSources(ctx, tenantName, ns)
	profiles := s.profileLookup(ctx)

	// Every name something is held under. A name that is an extension's key
	// or release is folded into the app that declares the extension; any
	// other name stands for itself, which is also how a purge would treat it.
	names := map[string]bool{}
	for name := range src.databases {
		names[name] = true
	}
	for key := range src.groups {
		names[extensionOwner(key, profiles)] = true
	}
	for key := range src.credentials {
		names[extensionOwner(key, profiles)] = true
	}
	for i := range src.claims {
		c := &src.claims[i]
		if app := c.Labels["gentianos.io/app"]; app != "" {
			names[app] = true
		}
		// A claim an uninstall kept still names the release it was made by,
		// in Helm's annotation; one a StatefulSet made carries the release
		// in the instance label its template gave it.
		for _, release := range []string{c.Annotations["meta.helm.sh/release-name"], c.Labels["app.kubernetes.io/instance"]} {
			if app, ok := strings.CutSuffix(release, "-release"); ok && app != "" {
				names[extensionOwner(app, profiles)] = true
			}
			if app, ok := strings.CutPrefix(release, ns+"-"); ok && app != "" {
				names[app] = true
			}
		}
	}

	out := &RetainedApps{Tenant: tenantName, Apps: []RetainedApp{}, Unknown: map[string]string{
		KindObjectStorage: unknownObjectStorage,
		KindCache:         unknownCache,
		KindDatabase:      unknownMariaDB,
	}}
	for kind, why := range src.failed {
		out.Unknown[kind] = why
	}

	for name := range names {
		if name == "" || installed[name] || backup.IsPlatformStore(name) {
			continue
		}
		profile, err := profiles(name)
		if err != nil {
			return nil, err
		}
		if profile != nil && profile.Annotations[platformAppAnnotation] == "true" {
			continue
		}
		removing := false
		for _, release := range releases {
			if ownsRelease(tenant, name, profile, release) {
				removing = true
				break
			}
		}
		if removing {
			continue
		}
		app := s.retainedApp(tenant, name, profile, src)
		for _, state := range app.Kinds {
			if state == RetainedPresent {
				out.Apps = append(out.Apps, app)
				break
			}
		}
	}
	sort.Slice(out.Apps, func(a, b int) bool { return out.Apps[a].Profile < out.Apps[b].Profile })
	return out, nil
}

// retainedApp says how each kind of data stands for one uninstalled app.
//
// The kinds this read cannot verify -- a bucket, a cache user -- are the ones
// a purge destroys before the ones it can, the credentials and the group. So
// an app whose purge stopped half-way is still listed here by what is left of
// the verifiable kinds, and does not drop out of the list with a bucket still
// to its name. The exception is an app whose profile is gone: a purge of it
// does not examine those kinds at all, says so in its answer, and the app is
// not listed afterwards.
func (s *Service) retainedApp(tenant *gentianov1alpha1.Tenant, name string, profile *gentianov1alpha1.ComponentProfile, src retainedSources) RetainedApp {
	tenantName := tenant.Name
	app := RetainedApp{Profile: name, State: AppStateRetained, ProfileAvailable: profile != nil, Kinds: map[string]string{}}
	stores := backup.ProfileStores(profile)
	state := func(kind string, present bool) string {
		switch {
		case present:
			return RetainedPresent
		case src.failed[kind] != "":
			return RetainedUnknown
		default:
			return RetainedAbsent
		}
	}

	// Database. The record is PostgreSQL's; for another engine, or none
	// declared, its absence says nothing or everything respectively.
	switch {
	case src.databases[name]:
		app.Kinds[KindDatabase] = RetainedPresent
	case stores.Database == gentianov1alpha1.DatabaseEngineMariaDB:
		app.Kinds[KindDatabase] = RetainedUnknown
	default:
		app.Kinds[KindDatabase] = state(KindDatabase, false)
	}

	declared := func(has bool) string {
		if profile == nil || has {
			return RetainedUnknown
		}
		return RetainedAbsent
	}
	app.Kinds[KindObjectStorage] = declared(stores.S3)
	app.Kinds[KindCache] = declared(stores.Redis)

	// Files: the claims a purge would delete.
	app.Volumes, _ = appVolumes(src.claims, tenant, name, profile)
	sort.Strings(app.Volumes)
	app.Kinds[KindFiles] = state(KindFiles, len(app.Volumes) > 0)

	// Credentials: the paths a purge would delete.
	held := false
	for _, path := range credentialPaths(tenant, name, backup.SidecarNames(profile)) {
		if src.credentials[strings.TrimPrefix(path, secrets.AppsPath(tenantName)+"/")] {
			held = true
		}
	}
	app.Kinds[KindCredentials] = state(KindCredentials, held)

	// Access groups: the ones a purge would delete.
	grouped := false
	for _, key := range ownedKeys(tenant, name, backup.SidecarNames(profile)) {
		grouped = grouped || src.groups[key]
	}
	app.Kinds[KindAccessGroup] = state(KindAccessGroup, grouped)
	return app
}

// retainedSources reads what is held, once, for the whole tenant. A source
// that cannot be read is recorded as failed and the read goes on: one
// unreachable service must not hide what the others report.
func (s *Service) retainedSources(ctx context.Context, tenantName, ns string) retainedSources {
	src := retainedSources{
		databases: map[string]bool{}, credentials: map[string]bool{}, groups: map[string]bool{},
		failed: map[string]string{},
	}

	// PostgreSQL databases, by the record the tenant's provisioning made and
	// a purge deletes last.
	dbs := &unstructured.UnstructuredList{}
	dbs.SetGroupVersionKind(cnpgDatabaseGVK.GroupVersion().WithKind("DatabaseList"))
	err := s.client.List(ctx, dbs, client.InNamespace(layout.System("postgresql")),
		client.MatchingLabels{meta.TenantLabel: tenantName})
	switch {
	case err == nil:
		for i := range dbs.Items {
			if app := dbs.Items[i].GetLabels()["gentianos.io/app"]; app != "" {
				src.databases[app] = true
			}
		}
	case apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		// No database operator on this cluster, so no databases of its.
	default:
		src.failed[KindDatabase] = "the database records could not be listed: " + err.Error()
	}

	// Stored credentials: the names below the tenant's apps path.
	if s.vault == nil {
		src.failed[KindCredentials] = "the operator has no connection to the vault (BAO_ADDR is not set)"
	} else if keys, err := s.vault.ListChildren(ctx, secrets.AppsPath(tenantName)); err != nil {
		src.failed[KindCredentials] = "the vault could not be listed: " + err.Error()
	} else {
		for _, key := range keys {
			src.credentials[strings.TrimSuffix(key, "/")] = true
		}
	}

	// Access groups. The realm is the tenant's name, as it is wherever this
	// API talks to the identity provider.
	prefix := keycloak.TenantAppGroup(tenantName, "")
	if groups, err := s.accessGroups(ctx); err != nil {
		src.failed[KindAccessGroup] = "the identity provider could not be asked: " + err.Error()
	} else if found, err := groups.GroupNames(ctx, tenantName, prefix); err != nil {
		src.failed[KindAccessGroup] = "the identity provider's groups could not be listed: " + err.Error()
	} else {
		for _, g := range found {
			src.groups[strings.TrimPrefix(g, prefix)] = true
		}
	}

	// Volume claims in the tenant's namespace.
	pvcs, err := s.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		src.failed[KindFiles] = "the volume claims could not be listed: " + err.Error()
	} else if err == nil {
		src.claims = pvcs.Items
	}
	return src
}

// profileLookup returns a ComponentProfile by name, nil when the cluster has
// none of that name, remembering each answer for the length of one read.
func (s *Service) profileLookup(ctx context.Context) func(string) (*gentianov1alpha1.ComponentProfile, error) {
	cache := map[string]*gentianov1alpha1.ComponentProfile{}
	return func(name string) (*gentianov1alpha1.ComponentProfile, error) {
		if cp, ok := cache[name]; ok {
			return cp, nil
		}
		cp := &gentianov1alpha1.ComponentProfile{}
		err := s.client.Get(ctx, client.ObjectKey{Name: name}, cp)
		switch {
		case apierrors.IsNotFound(err):
			cp = nil
		case err != nil:
			return nil, fmt.Errorf("get componentprofile %q: %w", name, err)
		}
		cache[name] = cp
		return cp, nil
	}
}

// extensionOwner returns the app a key belongs to: the app that declares an
// extension the key is made from ("{app}-{extension}"), or the key itself.
//
// Only a declaration folds a key into an app. A purge deletes an extension's
// path when the profile declares the extension and not otherwise, so a key
// that merely begins with another app's name is reported under its own: that
// is the name a purge would have to be asked for to remove it.
func extensionOwner(key string, profiles func(string) (*gentianov1alpha1.ComponentProfile, error)) string {
	if own, err := profiles(key); err == nil && own != nil {
		return key
	}
	for i := len(key) - 1; i > 0; i-- {
		if key[i] != '-' {
			continue
		}
		profile, err := profiles(key[:i])
		if err != nil || profile == nil {
			continue
		}
		for _, ext := range backup.SidecarNames(profile) {
			if ext == key[i+1:] {
				return key[:i]
			}
		}
	}
	return key
}
