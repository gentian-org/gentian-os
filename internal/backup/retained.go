/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The data uninstalled apps left.
//
// Uninstalling an app keeps its data, on purpose, and the app is then in no
// list of what the tenant has installed. Three acts have to find that data
// all the same, and must find the same: the read of what uninstalled apps
// hold, which shows it; an export, which copies it; and a purge, which
// destroys it. What says the data is there is durable and is the same for
// the three -- the record of what was provisioned, the database records the
// cluster keeps, and the volume claims in the tenant's namespace -- and this
// file is the one place that reads an app's name out of them.

// Held is what the cluster holds for a tenant that carries data, by the name
// it is held under, before any of it is attributed to an app.
type Held struct {
	// Provisioned is the record of the stores provisioning made, by app.
	Provisioned map[string]Provisioned
	// Databases are the apps a PostgreSQL database record names.
	Databases map[string]bool
	// Claims are the volume claims in the tenant's namespace.
	Claims []corev1.PersistentVolumeClaim
}

// Profiles answers an app's ComponentProfile, nil when the cluster has none
// of that name.
type Profiles func(name string) (*gentianov1alpha1.ComponentProfile, error)

// ExtensionOwner returns the app a key belongs to: the app that declares an
// extension the key is made from ("{app}-{extension}"), or the key itself.
//
// Only a declaration folds a key into an app. A purge deletes what is an
// extension's when the profile declares the extension and not otherwise, so
// a key that merely begins with another app's name stands for itself: that
// is the name a purge would have to be asked for to remove it.
func ExtensionOwner(key string, profiles Profiles) string {
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
		for _, ext := range SidecarNames(profile) {
			if ext == key[i+1:] {
				return key[:i]
			}
		}
	}
	return key
}

// DataHolders are the names data is held under for a tenant: every app a
// store is on record for, and every app a volume claim says it is of. A
// name that is an extension's release is folded into the app that declares
// the extension. Installed apps are among them; whoever asks leaves those
// out.
func DataHolders(tenant *gentianov1alpha1.Tenant, held Held, profiles Profiles) []string {
	ns := TenantNamespace(tenant)
	names := map[string]bool{}
	for name := range held.Databases {
		names[name] = true
	}
	for name := range held.Provisioned {
		names[name] = true
	}
	for i := range held.Claims {
		c := &held.Claims[i]
		if app := c.Labels["gentianos.io/app"]; app != "" {
			names[app] = true
		}
		// A claim an uninstall kept still names the release it was made by,
		// in Helm's annotation; one a StatefulSet made carries the release
		// in the instance label its template gave it.
		for _, release := range []string{c.Annotations["meta.helm.sh/release-name"], c.Labels["app.kubernetes.io/instance"]} {
			if app, ok := strings.CutSuffix(release, "-release"); ok && app != "" {
				names[ExtensionOwner(app, profiles)] = true
			}
			if app, ok := strings.CutPrefix(release, ns+"-"); ok && app != "" {
				names[app] = true
			}
		}
	}
	delete(names, "")
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HeldStores are the stores the cluster holds for an app that is not
// installed: the ones the record of what was provisioned names, and a
// PostgreSQL database when the cluster keeps a database record for the app
// and the record names no engine.
//
// What the app's profile declares is not asked. A profile says what an
// install would make; these are the stores that were made and not
// destroyed, which is what there is to copy and to put back into. (A purge
// adds the profile's to them, MergeStores: destroying what is not there
// costs nothing, and copying it fails.)
func HeldStores(app string, held Held) Stores {
	stores := MergeStores(Stores{}, held.Provisioned[app])
	if stores.Database == "" && held.Databases[app] {
		stores.Database = gentianov1alpha1.DatabaseEnginePostgreSQL
	}
	return stores
}
