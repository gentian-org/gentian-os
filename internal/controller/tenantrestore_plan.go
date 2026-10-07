/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// What a restore puts back is what the bundle says it holds.
//
// A restore used to go by the tenant it was restoring into: it listed that
// tenant's installed apps, derived each database, bucket and claim name from
// it, and fetched whatever artefact that name spelled. The bundle's manifest
// -- the one part of a bundle written to say what is in it -- was never read.
// So an app the bundle held and the tenant did not have installed was dropped
// without a word; an app the tenant had and the bundle did not was "restored"
// from an artefact that was not there; and a bundle brought in under another
// tenant name could not be found at all, because its artefacts are named
// after the tenant it was taken of.
//
// The plan below is made once, before anything is changed, from the manifest
// and the tenant as it stands. The rule it applies:
//
//  1. Only an app the manifest lists is ever touched. An app the tenant has
//     and the bundle does not is left exactly as it is.
//  2. An app the manifest lists is restored whole or not at all. It is not
//     restored when the tenant does not have it installed; when its
//     ComponentProfile is not on the cluster; when the build installed is
//     older than the one that wrote the data, or cannot be compared with it
//     (unless spec.skipVersionCheck); when the bundle holds a store of a kind
//     or engine the installed app does not have; or when it holds a volume
//     claim the tenant does not have, or has as another app's.
//  3. Every app not restored is named, with the reason, in
//     status.notRestored, and the restore ends with status.complete false.
//     Nothing the bundle holds is dropped without saying so.
//  4. With spec.apps, only the apps named are considered, and each has to be
//     restorable: otherwise the restore is refused before anything is
//     changed. A person who names an app is not answered with the others.
//
// Where an artefact is, and what it was captured from, is the manifest's to
// say (format 2). Where it goes is the tenant's: the database, bucket or
// claim of that kind the installed app has, by the inventory's names -- which
// differ from the bundle's whenever the tenant's name or prefixes do.

// restorePlan is what one restore will do.
type restorePlan struct {
	// apps are restored, in the manifest's order.
	apps []plannedApp
	// tenantWide are the realm and the desktop's database.
	tenantWide []gentianov1alpha1.BundleArtefact
	// notRestored are the apps the bundle holds that are not restored.
	notRestored []gentianov1alpha1.RestoreOmission
	// derivation is where the names came from.
	derivation string
	// schemaVersion is the manifest's.
	schemaVersion int
}

// plannedApp is one app that will be restored.
type plannedApp struct {
	name      string
	artefacts []gentianov1alpha1.BundleArtefact
	// note is said on the app's entry: something true about this restore of
	// it that whoever ran it should know.
	note string
}

// liveApp is an app as the tenant has it now.
type liveApp struct {
	// installed is whether the tenant lists the app.
	installed bool
	// profile is its ComponentProfile, nil when the cluster has none.
	profile *gentianov1alpha1.ComponentProfile
	// claims are the volume claims that are the app's (backup.AppVolumes, or
	// the profile's explicit list).
	claims []string
}

// errRestoreRefused is a restore that is not begun.
type errRestoreRefused struct {
	reason  string
	message string
}

func (e *errRestoreRefused) Error() string { return e.message }

func refuseRestore(reason, format string, args ...any) error {
	return &errRestoreRefused{reason: reason, message: fmt.Sprintf(format, args...)}
}

// planRestore decides what a restore does, and changes nothing.
func planRestore(
	m *backup.Manifest,
	tenant *gentianov1alpha1.Tenant,
	wanted []string,
	skipVersionCheck bool,
	live func(app string) (liveApp, error),
) (*restorePlan, error) {
	plan := &restorePlan{schemaVersion: m.SchemaVersion, derivation: gentianov1alpha1.RestoreNamesFromManifest}
	if !m.NamesArtefacts() {
		plan.derivation = gentianov1alpha1.RestoreNamesDerived
	}
	// The tenant the bundle was taken of, for the names a format 1 manifest
	// does not give: its artefacts are filed under that tenant's names.
	source := &gentianov1alpha1.Tenant{}
	source.Name = m.Tenant
	if m.TenantSpec != nil {
		source.Spec = *m.TenantSpec
	}

	held := map[string]bool{}
	for _, app := range m.Apps {
		if app.Name == bundle.TenantComponent || app.Name == "" {
			continue
		}
		held[app.Name] = true
		if len(wanted) > 0 && !slices.Contains(wanted, app.Name) {
			continue
		}
		now, err := live(app.Name)
		if err != nil {
			return nil, err
		}
		artefacts, note, why, err := planApp(m, source, tenant, app, now, skipVersionCheck)
		if err != nil {
			return nil, err
		}
		if why != "" {
			plan.notRestored = append(plan.notRestored, gentianov1alpha1.RestoreOmission{App: app.Name, Reason: why})
			continue
		}
		plan.apps = append(plan.apps, plannedApp{name: app.Name, artefacts: artefacts, note: note})
	}

	// An app asked for by name is restored or the restore is refused.
	if len(wanted) > 0 {
		var problems []string
		for _, name := range wanted {
			if !held[name] {
				problems = append(problems, fmt.Sprintf("%s is not in the bundle", name))
			}
		}
		for _, o := range plan.notRestored {
			problems = append(problems, fmt.Sprintf("%s %s", o.App, o.Reason))
		}
		if len(problems) > 0 {
			names := make([]string, 0, len(held))
			for name := range held {
				names = append(names, name)
			}
			sort.Strings(names)
			return nil, refuseRestore("CannotRestoreAsAsked",
				"spec.apps names what this restore cannot put back: %s. Nothing was changed. The bundle holds: %s",
				strings.Join(problems, "; "), strings.Join(names, ", "))
		}
	}

	// The tenant's own: the realm and the desktop's database, each where
	// the manifest says it is.
	if m.Identity != nil && m.Identity.Path != "" {
		if err := cleanArtefactPath(m.Identity.Path); err != nil {
			return nil, err
		}
		plan.tenantWide = append(plan.tenantWide, gentianov1alpha1.BundleArtefact{
			Kind: bundle.ArtefactIdentity, Name: m.Identity.Realm, Path: m.Identity.Path,
			Target: keycloakRealmName(tenant),
		})
	}
	if m.Shell != nil && m.Shell.Path != "" {
		if err := cleanArtefactPath(m.Shell.Path); err != nil {
			return nil, err
		}
		plan.tenantWide = append(plan.tenantWide, gentianov1alpha1.BundleArtefact{
			Kind: bundle.ArtefactPostgres, Name: m.Shell.Name, Path: m.Shell.Path,
			Target: databaseName(tenant, portalShellAppName),
		})
	}

	if len(plan.apps) == 0 {
		if len(plan.notRestored) == 0 {
			return nil, refuseRestore("NothingToRestore", "the bundle's manifest lists no app; there is nothing to restore")
		}
		return nil, refuseRestore("NothingToRestore",
			"none of the apps this bundle holds can be restored here: %s. Nothing was changed",
			omissionsText(plan.notRestored))
	}
	return plan, nil
}

// planApp decides one app: its artefacts with where each goes, or why it is
// not restored. An error is a manifest that cannot be trusted at all.
func planApp(
	m *backup.Manifest,
	source, tenant *gentianov1alpha1.Tenant,
	app backup.ManifestApp,
	now liveApp,
	skipVersionCheck bool,
) (artefacts []gentianov1alpha1.BundleArtefact, note, why string, err error) {
	if !now.installed {
		return nil, "", "is not installed in this tenant; install it, then restore it by naming it in spec.apps", nil
	}
	if now.profile == nil {
		return nil, "", "has no ComponentProfile on this cluster, so what it owns here cannot be determined", nil
	}
	note, why = buildRule(app, now.profile, tenantAppDigest(tenant, app.Name), skipVersionCheck)
	if why != "" {
		return nil, "", why, nil
	}

	inv := backup.InventoryOf(tenant, app.Name, now.profile)
	stores := app.Stores
	if !m.NamesArtefacts() {
		stores = derivedStores(source, app, now)
	}
	covered := map[backup.Kind]bool{}
	for _, s := range stores {
		if err := cleanArtefactPath(s.Path); err != nil {
			return nil, "", "", fmt.Errorf("app %s: %w", app.Name, err)
		}
		a := gentianov1alpha1.BundleArtefact{Kind: s.Kind, Name: s.Name, Path: s.Path, Release: s.Release}
		switch s.Kind {
		case bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned:
			if inv.Stores.Database != gentianov1alpha1.DatabaseEnginePostgreSQL {
				return nil, "", fmt.Sprintf("has a PostgreSQL database in the bundle and %s here", engineText(inv.Stores.Database)), nil
			}
			a.Target = inv.Database
			covered[backup.KindDatabase] = true
		case bundle.ArtefactMariaDB:
			if inv.Stores.Database != gentianov1alpha1.DatabaseEngineMariaDB {
				return nil, "", fmt.Sprintf("has a MariaDB database in the bundle and %s here", engineText(inv.Stores.Database)), nil
			}
			a.Target = inv.Database
			covered[backup.KindDatabase] = true
		case bundle.ArtefactS3:
			if !inv.Stores.S3 {
				return nil, "", "has a bucket in the bundle and the installed app has no object storage", nil
			}
			a.Target = inv.Bucket
			covered[backup.KindObjectStorage] = true
		case bundle.ArtefactVolume:
			// A claim keeps its name: it is named by the app's chart and its
			// release, neither of which carries the tenant's.
			if !slices.Contains(now.claims, s.Name) {
				return nil, "", fmt.Sprintf("has the volume claim %s in the bundle, which is not one of the app's claims here (%s)",
					s.Name, listOrNone(now.claims)), nil
			}
			a.Target = s.Name
			covered[backup.KindFiles] = true
		default:
			return nil, "", fmt.Sprintf("has an artefact of kind %q in the bundle, which this platform does not know how to restore", s.Kind), nil
		}
		artefacts = append(artefacts, a)
	}

	// What the installed app has that the bundle does not hold is left as it
	// is, and said.
	var untouched []string
	if inv.Stores.Database != "" && !covered[backup.KindDatabase] {
		untouched = append(untouched, "database")
	}
	if inv.Stores.S3 && !covered[backup.KindObjectStorage] {
		untouched = append(untouched, "bucket")
	}
	var claimsLeft []string
	for _, claim := range now.claims {
		if !slices.ContainsFunc(artefacts, func(a gentianov1alpha1.BundleArtefact) bool {
			return a.Kind == bundle.ArtefactVolume && a.Target == claim
		}) {
			claimsLeft = append(claimsLeft, claim)
		}
	}
	if len(claimsLeft) > 0 {
		untouched = append(untouched, "volume claim(s) "+strings.Join(claimsLeft, ", "))
	}
	if len(untouched) > 0 {
		note = joinNotes(note, "the bundle holds nothing for its "+strings.Join(untouched, ", its ")+"; left as it is")
	}
	return artefacts, note, "", nil
}

// buildRule compares the build that wrote an app's data with the build that
// is installed. It answers a note for a restore that goes ahead across a
// difference, or why it does not go ahead.
//
// The same chart version is the same build as far as the data goes. A newer
// one installed is restored: an app upgrades older data when it starts,
// which is what restoring an old backup always relies on. An older one, or a
// version that cannot be compared, is not -- the failure is silent, an older
// app serving data written by a newer schema -- unless the restore says to
// skip the check.
func buildRule(app backup.ManifestApp, profile *gentianov1alpha1.ComponentProfile, digest string, skip bool) (note, why string) {
	wrote, installed := app.ChartVersion, profileChartVersion(profile)
	if wrote == installed {
		if app.Digest != "" && digest != "" && app.Digest != digest {
			return fmt.Sprintf("its data was written by another build of version %s (%s; %s is installed)", wrote, app.Digest, digest), ""
		}
		return "", ""
	}
	if wrote == "" || installed == "" {
		// One side records no chart version: an entry that is not delivered
		// as a chart, or a manifest that did not say.
		if skip {
			return "the version that wrote its data and the one installed could not be compared; restored because spec.skipVersionCheck is set", ""
		}
		return "", fmt.Sprintf("was written by version %q and version %q is installed, which cannot be compared; set spec.skipVersionCheck to restore it anyway",
			wrote, installed)
	}
	w, werr := version.ParseGeneric(wrote)
	i, ierr := version.ParseGeneric(installed)
	switch {
	case werr == nil && ierr == nil && w.LessThan(i):
		return fmt.Sprintf("its data was written by version %s and version %s is installed; the app upgrades it when it starts", wrote, installed), ""
	case skip:
		return fmt.Sprintf("its data was written by version %s and version %s is installed; restored because spec.skipVersionCheck is set", wrote, installed), ""
	case werr != nil || ierr != nil:
		return "", fmt.Sprintf("was written by version %s and version %s is installed, which cannot be compared; set spec.skipVersionCheck to restore it anyway",
			wrote, installed)
	default:
		return "", fmt.Sprintf("was written by version %s and the older version %s is installed; an app cannot be trusted to read data a newer one wrote. "+
			"Install version %s or later, or set spec.skipVersionCheck", wrote, installed, wrote)
	}
}

// derivedStores is what a format 1 manifest's app entry means: it names the
// kinds captured and nothing more, so the artefacts are the ones the tenant
// the bundle was taken of would have produced, by the inventory's names for
// that tenant. Volumes are not named even so; the claims the app has now are
// taken to be the ones captured, and a claim the bundle turns out not to
// hold fails its restore Job, loudly.
func derivedStores(source *gentianov1alpha1.Tenant, app backup.ManifestApp, now liveApp) []backup.ManifestStore {
	var out []backup.ManifestStore
	seen := map[string]bool{}
	for _, s := range app.Stores {
		if seen[s.Kind] {
			continue
		}
		seen[s.Kind] = true
		switch s.Kind {
		case bundle.ArtefactPostgres:
			db := backup.DatabaseName(source, app.Name)
			out = append(out, backup.ManifestStore{Kind: s.Kind, Name: db, Path: backup.PostgresArtefact(db)})
		case bundle.ArtefactMariaDB:
			db := backup.DatabaseName(source, app.Name)
			out = append(out, backup.ManifestStore{Kind: s.Kind, Name: db, Path: backup.MariaDBArtefact(db)})
		case bundle.ArtefactS3:
			bucket := backup.S3Bucket(source, app.Name)
			out = append(out, backup.ManifestStore{Kind: s.Kind, Name: bucket, Path: backup.S3Artefact(bucket)})
		case bundle.ArtefactVolume:
			for _, claim := range now.claims {
				out = append(out, backup.ManifestStore{Kind: s.Kind, Name: claim, Path: backup.VolumeArtefact(claim)})
			}
		default:
			out = append(out, s)
		}
	}
	return out
}

// cleanArtefactPath refuses a path that leaves the bundle. The manifest is
// read from the bundle, and a bundle can come from anywhere: its paths are
// joined onto the bundle's prefix and fetched with the operator's storage
// credentials.
func cleanArtefactPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") ||
		strings.ContainsAny(p, "'\"`$\\\n ") {
		return refuseRestore("BundleUnusable", "the bundle's manifest names an artefact path that is not a plain path inside the bundle: %q", p)
	}
	return nil
}

func engineText(engine gentianov1alpha1.DatabaseEngine) string {
	if engine == "" {
		return "no database"
	}
	return "a " + string(engine) + " database"
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "it has none"
	}
	return strings.Join(items, ", ")
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func omissionsText(omitted []gentianov1alpha1.RestoreOmission) string {
	parts := make([]string, 0, len(omitted))
	for _, o := range omitted {
		parts = append(parts, o.App+" "+o.Reason)
	}
	return strings.Join(parts, "; ")
}

// restoreLimits are what no restore brings back, said on every result.
//
// A bundle holds no stored credential, on purpose: it would put every
// password of a tenant in a file that leaves the cluster. What the platform
// itself seeds -- each store's password, an app's generated secrets -- the
// tenant being restored into already has, derived from this cluster's master
// password when it was provisioned, and a restore changes none of it. What a
// person typed in is nowhere but the vault it was typed into.
func restoreLimits(derivation string) []string {
	notes := []string{
		"Stored credentials are not in a bundle. The ones the platform seeds were made for this tenant when it was provisioned and a restore changes none of them. " +
			"Credentials a person entered -- a repository's password, an SMTP relay's, an API key typed into an app's settings in the vault -- did not come back and have to be entered again.",
		"Data an app encrypted with a secret the platform generated for it can be read only where that secret is the same: on the cluster the bundle was taken on, or one built from its recovery kit.",
		"Members came back without passwords and have to be sent a reset. Mail, app grants, integration bindings and the cache are not in a bundle: grants and bindings are declared state and come from where the tenant is declared.",
	}
	if derivation == gentianov1alpha1.RestoreNamesDerived {
		notes = append(notes, "This bundle's manifest is of format 1, which names apps and kinds only: the artefact names were derived from the tenant the manifest records, "+
			"and the volume claims restored are the ones the apps have now. Databases an app created besides its own are not in a format 1 bundle.")
	}
	return notes
}
