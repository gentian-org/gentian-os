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
// values) from the vault, group names from the identity provider. It runs
// nothing. A bucket, a cache user and a MariaDB database cannot be asked of
// the API server, and were reported as unknown for that reason; provisioning
// now records each store it makes (backup.Provisioned), a purge takes the
// entry out when it has destroyed the store, and this read answers from the
// record. A kind is unknown only when what would say could not be read this
// time.

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
	// that depend on that are unknown here, and a purge of the app is
	// refused until the profile is back.
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

// sourceProvisioned names the record of what was provisioned among the
// sources that can fail. It is not a kind and is not reported as one.
const sourceProvisioned = "provisioned"

// retainedSources is what the cluster holds for a tenant, by the name it is
// held under, before any of it is attributed to an app.
type retainedSources struct {
	// databases are the apps a PostgreSQL database is recorded for.
	databases map[string]bool
	// provisioned is the record of the stores provisioning made, by app.
	provisioned map[string]backup.Provisioned
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
	ns := tenantNamespace(tenant)

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

	src := s.retainedSources(ctx, tenant, ns)
	profiles := s.profileLookup(ctx)

	// Every name something is held under. The names data is held under --
	// a store on record, a volume claim -- are read by the one rule an
	// export copies that data by (backup.DataHolders); credentials and
	// access groups are added here, which hold no data a bundle carries. A
	// name that is an extension's key or release is folded into the app that
	// declares the extension; any other name stands for itself, which is
	// also how a purge would treat it.
	names := map[string]bool{}
	for _, name := range backup.DataHolders(tenant, backup.Held{
		Provisioned: src.provisioned, Databases: src.databases, Claims: src.claims,
	}, backup.Profiles(profiles)) {
		names[name] = true
	}
	for key := range src.groups {
		names[extensionOwner(key, profiles)] = true
	}
	for key := range src.credentials {
		names[extensionOwner(key, profiles)] = true
	}

	out := &RetainedApps{Tenant: tenantName, Apps: []RetainedApp{}, Unknown: map[string]string{}}
	for kind, why := range src.failed {
		if kind != sourceProvisioned {
			out.Unknown[kind] = why
		}
	}
	// The record is what says a bucket, a cache user or a MariaDB database
	// is there; unread, each of them is unknown.
	if why := src.failed[sourceProvisioned]; why != "" {
		out.Unknown[KindObjectStorage], out.Unknown[KindCache] = why, why
		if out.Unknown[KindDatabase] == "" {
			out.Unknown[KindDatabase] = why
		}
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
// to its name.
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

	// The stores. Each is present when the record of what was provisioned
	// names it -- for a PostgreSQL database also when the cluster's own
	// Database object does -- and absent when the record was read and does
	// not. When the record could not be read, a store the profile declares,
	// or any store of an app whose profile is gone, is unknown.
	recorded := src.provisioned[name]
	recordUnread := src.failed[sourceProvisioned] != ""
	store := func(kind string, present, declared bool) string {
		switch {
		case present:
			return RetainedPresent
		case recordUnread && (declared || profile == nil):
			return RetainedUnknown
		case kind == KindDatabase && src.failed[KindDatabase] != "":
			// The cluster's own Database objects could not be listed.
			return RetainedUnknown
		default:
			return RetainedAbsent
		}
	}
	app.Kinds[KindDatabase] = store(KindDatabase,
		src.databases[name] || recorded.Has(backup.KindDatabase), stores.Database != "")
	app.Kinds[KindObjectStorage] = store(KindObjectStorage, recorded.Has(backup.KindObjectStorage), stores.S3)
	app.Kinds[KindCache] = store(KindCache, recorded.Has(backup.KindCache), stores.Redis)
	// The key at the model gateway: on record for an app whose profile
	// declared the gateway on a cluster that served models -- and for any app
	// installed before the gateway had to be declared, until the reconciler
	// has taken its key away.
	app.Kinds[KindModelAccess] = store(KindModelAccess, recorded.Has(backup.KindModelAccess), backup.DeclaresModelAccess(profile))

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
func (s *Service) retainedSources(ctx context.Context, tenant *gentianov1alpha1.Tenant, ns string) retainedSources {
	tenantName := tenant.Name
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

	// Every store provisioning made, by the record it keeps of them.
	if recorded, err := s.provisioned(ctx, tenantName); err != nil {
		src.failed[sourceProvisioned] = "the record of what was provisioned could not be read: " + err.Error()
	} else {
		src.provisioned = recorded
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

	// Access groups, in the tenant's own realm.
	prefix := keycloak.TenantAppGroup(tenantName, "")
	if groups, err := s.accessGroups(ctx); err != nil {
		src.failed[KindAccessGroup] = "the identity provider could not be asked: " + err.Error()
	} else if found, err := groups.GroupNames(ctx, keycloak.RealmName(tenant), prefix); err != nil {
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

// extensionOwner returns the app a key belongs to: the shared inventory's
// rule (backup.ExtensionOwner).
func extensionOwner(key string, profiles func(string) (*gentianov1alpha1.ComponentProfile, error)) string {
	return backup.ExtensionOwner(key, profiles)
}
