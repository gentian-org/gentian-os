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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//  5. An app the manifest holds as retained -- uninstalled when the bundle
//     was taken, its data kept -- is put back as retained: into the stores
//     the tenant still holds for it, with nothing installed and nothing
//     paused. Where the tenant holds none of it any more, it was purged, and
//     a purge is for good: the app is not restored and is named. Only into a
//     tenant made new for the bundle (spec.intoNewTenant) are its stores
//     made, so that an import brings what the tenant it was taken of had.
//     Where the tenant has since installed the app again, the data is the
//     installed app's and is restored by rule 2.
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
	// notIncluded is what the manifest says the bundle does not hold.
	notIncluded []string
	// sourceTenant is the tenant the bundle was taken of.
	sourceTenant string
	// rights is what is done with the bundle's entries of the rights store.
	rights *gentianov1alpha1.RestoreRights
	// archivedMailboxes are the archived mailboxes the bundle holds, as
	// they are put on record where the mailboxes are put back.
	archivedMailboxes []gentianov1alpha1.RestoredArchive
	// notes are said on the result: what of the tenant's own the bundle
	// holds and this restore does not put back, and why.
	notes []string
}

// planTarget is the tenant restored into, as far as the plan needs to know
// it beyond its apps: where what is the tenant's own goes.
type planTarget struct {
	// intoNewTenant is spec.intoNewTenant: an import.
	intoNewTenant bool
	// desktopDatabase is the desktop's database: the tenant's on the
	// tenants' PostgreSQL, or the kernel's for the tenant that adopts the
	// kernel realm.
	desktopDatabase string
	// mailDomain is the tenant's mail domain where the cluster keeps its
	// mailboxes and they are the tenant's alone; noMailboxes says why not,
	// where it is empty.
	mailDomain  string
	noMailboxes string
	// cluster is this cluster's id in the rights store, and haveRights
	// whether the operator has a store to write to.
	cluster    string
	haveRights bool
}

// plannedApp is one app that will be restored.
type plannedApp struct {
	name      string
	artefacts []gentianov1alpha1.BundleArtefact
	// note is said on the app's entry: something true about this restore of
	// it that whoever ran it should know.
	note string
	// retained says the app is put back as retained: not installed, not
	// paused, its stores made where the plan says so.
	retained bool
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
	// held is what the tenant holds for an app that is not installed: the
	// stores (backup.HeldStores); the claims are in claims.
	held backup.Stores
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
	target planTarget,
	live func(app string) (liveApp, error),
) (*restorePlan, error) {
	plan := &restorePlan{schemaVersion: m.SchemaVersion, derivation: gentianov1alpha1.RestoreNamesFromManifest,
		notIncluded: m.NotIncluded, sourceTenant: m.Tenant}
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
		var artefacts []gentianov1alpha1.BundleArtefact
		var note, why string
		retained := app.Retained && !now.installed
		if retained {
			artefacts, note, why, err = planRetained(tenant, app, now, target.intoNewTenant)
		} else {
			artefacts, note, why, err = planApp(m, source, tenant, app, now, skipVersionCheck)
			if app.Retained && why == "" {
				note = joinNotes(note, "the bundle holds its data as an uninstalled app's, and the app is installed here now: the data is restored into the installed app")
			}
		}
		if err != nil {
			return nil, err
		}
		if why != "" {
			plan.notRestored = append(plan.notRestored, gentianov1alpha1.RestoreOmission{App: app.Name, Reason: why})
			continue
		}
		plan.apps = append(plan.apps, plannedApp{name: app.Name, artefacts: artefacts, note: note, retained: retained})
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

	// The tenant's own, each where the manifest says it is, kind by kind
	// (bundle.TenantArtefacts).
	for _, kind := range bundle.TenantArtefacts {
		switch kind {
		case bundle.ArtefactPostgres:
			if m.Shell == nil || m.Shell.Path == "" {
				continue
			}
			if err := cleanArtefactPath(m.Shell.Path); err != nil {
				return nil, err
			}
			plan.tenantWide = append(plan.tenantWide, gentianov1alpha1.BundleArtefact{
				Kind: kind, Name: m.Shell.Name, Path: m.Shell.Path, Target: target.desktopDatabase,
			})
		case bundle.ArtefactMailboxes:
			if m.Mailboxes == nil || m.Mailboxes.Path == "" {
				continue
			}
			if err := cleanArtefactPath(m.Mailboxes.Path); err != nil {
				return nil, err
			}
			if target.mailDomain == "" {
				// Said, not dropped: the bundle holds mail this restore
				// has nowhere to put.
				why := target.noMailboxes
				if why == "" {
					why = "this cluster runs no mail server of its own, so there are no mailboxes to put them into"
				}
				plan.notes = append(plan.notes, "The bundle holds the mailboxes of "+m.Mailboxes.Name+", and they were NOT put back: "+why+".")
				continue
			}
			plan.tenantWide = append(plan.tenantWide, gentianov1alpha1.BundleArtefact{
				Kind: kind, Name: m.Mailboxes.Name, Path: m.Mailboxes.Path, Target: target.mailDomain,
			})
			plan.archivedMailboxes = archivedMailboxesPlan(m.ArchivedMailboxes, target.mailDomain)
		case bundle.ArtefactIdentity:
			if m.Identity == nil || m.Identity.Path == "" {
				continue
			}
			if err := cleanArtefactPath(m.Identity.Path); err != nil {
				return nil, err
			}
			plan.tenantWide = append(plan.tenantWide, gentianov1alpha1.BundleArtefact{
				Kind: kind, Name: m.Identity.Realm, Path: m.Identity.Path, Target: keycloakRealmName(tenant),
			})
		case bundle.ArtefactRights:
			plan.rights = rightsPlan(m, tenant, target.cluster, target.intoNewTenant, target.haveRights)
		default:
			// A kind added to the list and not here is a kind a bundle
			// would hold and no restore put back.
			return nil, fmt.Errorf("a restore does not know what to do with a tenant's %q", kind)
		}
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
	holdsSecrets := false
	for _, s := range stores {
		if err := cleanArtefactPath(s.Path); err != nil {
			return nil, "", "", fmt.Errorf("app %s: %w", app.Name, err)
		}
		a := gentianov1alpha1.BundleArtefact{Kind: s.Kind, Name: s.Name, Path: s.Path, Release: s.Release}
		switch s.Kind {
		case bundle.ArtefactSecrets:
			if err := plannedSecrets(app, s); err != nil {
				return nil, "", "", err
			}
			// No target: where a value goes is never the bundle's to say.
			a.Name = app.Name
			holdsSecrets = true
		case bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned:
			if inv.Stores.Database != gentianov1alpha1.DatabaseEnginePostgreSQL {
				return nil, "", fmt.Sprintf("has a PostgreSQL database in the bundle and %s here", engineText(inv.Stores.Database)), nil
			}
			a.Target = inv.Database
			covered[backup.KindDatabase] = true
		case bundle.ArtefactMariaDB, bundle.ArtefactMariaDBOwned:
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
	note = joinNotes(note, secretsPlanNote(app.Name, now.profile, holdsSecrets))
	return artefacts, note, "", nil
}

// plannedSecrets checks an app's secrets artefact as a manifest names it:
// the app's own, at the one place an export writes it. A manifest comes from
// a bundle, and a bundle from anywhere; one that files another app's
// secrets under this app, or names a path of its own, is not gone by.
func plannedSecrets(app backup.ManifestApp, s backup.ManifestStore) error {
	if s.Name != app.Name || s.Path != backup.SecretsArtefact(app.Name) {
		return fmt.Errorf("app %s: the manifest names secrets %q at %q, which is not where this app's own are", app.Name, s.Name, s.Path)
	}
	return nil
}

// secretsPlanNote says, before anything is changed, what a restore does with
// the secrets the app's profile here has the platform generate.
func secretsPlanNote(appName string, profile *gentianov1alpha1.ComponentProfile, held bool) string {
	declared := declaredSecrets(appName, profile)
	switch {
	case held && len(declared) > 0:
		return "the bundle holds the secrets its data was written with: each one its profile declares here is set to the bundle's value, " +
			"replacing the stored one where they differ, and the app is restarted with them"
	case held:
		return "the bundle holds secrets of its own and its profile here declares none: none is written"
	case len(declared) > 0:
		return "the bundle holds none of its own secrets, which are left as they are: what the app encrypted with them is readable only if they are the ones the data was written with"
	}
	return ""
}

// planRetained decides an app the bundle holds as retained and the tenant
// does not have installed: its artefacts with where each goes, or why it is
// not put back.
//
// Each artefact goes into the store of that kind the tenant holds for the
// app, by the inventory's names. A store the tenant does not hold is made
// only for a tenant made new for the bundle; anywhere else it was purged
// since the bundle was taken, and the app -- whole, as every app is
// restored whole or not at all -- stays as it is.
func planRetained(
	tenant *gentianov1alpha1.Tenant,
	app backup.ManifestApp,
	now liveApp,
	intoNewTenant bool,
) (artefacts []gentianov1alpha1.BundleArtefact, note, why string, err error) {
	holdsNothing := now.held == (backup.Stores{}) && len(now.claims) == 0
	if holdsNothing && !intoNewTenant {
		return nil, "", "was uninstalled with its data kept when the bundle was taken, and this tenant holds none of that data any more: it was purged since, " +
			"and a purge is for good. Install the app, then restore it by naming it in spec.apps", nil
	}
	if now.profile == nil && intoNewTenant {
		return nil, "", "was uninstalled with its data kept when the bundle was taken, and has no ComponentProfile on this cluster: " +
			"nothing says how its stores are to be made here. Add the app's definition to a catalogue of this cluster and restore it by naming it in spec.apps", nil
	}
	declared := backup.ProfileStores(now.profile)
	holdsSecrets := false
	gone := func(what string) string {
		return fmt.Sprintf("was uninstalled with its data kept when the bundle was taken, and this tenant no longer holds its %s: it was purged since, and a purge is for good", what)
	}
	for _, s := range app.Stores {
		if err := cleanArtefactPath(s.Path); err != nil {
			return nil, "", "", fmt.Errorf("app %s: %w", app.Name, err)
		}
		a := gentianov1alpha1.BundleArtefact{Kind: s.Kind, Name: s.Name, Path: s.Path, Release: s.Release}
		engine := gentianov1alpha1.DatabaseEngine("")
		switch s.Kind {
		case bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned:
			engine = gentianov1alpha1.DatabaseEnginePostgreSQL
		case bundle.ArtefactMariaDB, bundle.ArtefactMariaDBOwned:
			engine = gentianov1alpha1.DatabaseEngineMariaDB
		}
		switch {
		case s.Kind == bundle.ArtefactSecrets:
			if err := plannedSecrets(app, s); err != nil {
				return nil, "", "", err
			}
			a.Name = app.Name
			holdsSecrets = true
		case engine != "":
			switch {
			case now.held.Database == engine:
			case now.held.Database != "":
				return nil, "", fmt.Sprintf("has a %s database in the bundle and this tenant holds %s for it", engine, engineText(now.held.Database)), nil
			case !intoNewTenant:
				return nil, "", gone("database"), nil
			case declared.Database != engine:
				// Made here it would be a database the app, installed
				// later, does not look for.
				return nil, "", fmt.Sprintf("has a %s database in the bundle and its definition on this cluster declares %s", engine, engineText(declared.Database)), nil
			}
			a.Target = backup.DatabaseName(tenant, app.Name)
		case s.Kind == bundle.ArtefactS3:
			switch {
			case now.held.S3:
			case !intoNewTenant:
				return nil, "", gone("bucket"), nil
			case !declared.S3:
				return nil, "", "has a bucket in the bundle and its definition on this cluster declares no object storage", nil
			}
			a.Target = backup.S3Bucket(tenant, app.Name)
		case s.Kind == bundle.ArtefactVolume:
			a.Target = s.Name
			if !slices.Contains(now.claims, s.Name) {
				switch {
				case !intoNewTenant:
					return nil, "", gone("volume claim " + s.Name), nil
				case s.Claim == nil || s.Claim.Size == "":
					return nil, "", fmt.Sprintf("has the volume claim %s in the bundle, which this tenant does not have, and the bundle does not say how to make it", s.Name), nil
				}
				// To be made, from what the bundle recorded of it.
				a.Claim = statusClaim(s.Claim)
			}
		default:
			return nil, "", fmt.Sprintf("has an artefact of kind %q in the bundle, which this platform does not know how to restore", s.Kind), nil
		}
		artefacts = append(artefacts, a)
	}
	if !slices.ContainsFunc(artefacts, func(a gentianov1alpha1.BundleArtefact) bool { return a.Kind != bundle.ArtefactSecrets }) {
		return nil, "", "is in the bundle as an uninstalled app, with no data", nil
	}
	note = "its data was put back as an uninstalled app's: the app is not installed, and installing it finds the data"
	switch own := declaredSecrets(app.Name, now.profile); {
	case holdsSecrets && len(own) > 0:
		note = joinNotes(note, "the bundle holds the secrets its data was written with: each one its profile declares here is set to the bundle's value, where its next install finds it")
	case holdsSecrets:
		note = joinNotes(note, "the bundle holds secrets of its own and no profile here declares them: none is written, and the app installed later makes new ones that cannot read what it encrypted")
	case len(own) > 0:
		note = joinNotes(note, "the bundle holds none of its own secrets: what the app encrypted with them is readable only if the ones here are the ones the data was written with")
	}
	for _, a := range artefacts {
		if a.Claim != nil {
			note = joinNotes(note, "its volume claims were made here, on this cluster's default storage class")
			break
		}
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
func restoreLimits(derivation string, schemaVersion int) []string {
	credentials := "Of the stored credentials a bundle holds one kind: the secrets the platform generated for an app at its profile's request, which the app's data was written with. " +
		"A restore sets them to the bundle's values; what is said of each app says which. Every other credential the platform seeds -- a database's, a bucket's, a sign-in client's -- " +
		"was made for this tenant when it was provisioned and a restore changes none of them. "
	encrypted := "Data an app encrypted with a secret the platform generated for it is readable where that secret was set from the bundle: an app whose secrets the bundle does not hold reads such data only where they are the ones it was written with."
	if schemaVersion < secretsSchemaVersion {
		credentials = fmt.Sprintf("Stored credentials are not in this bundle: it is of format %d, and a bundle holds an app's own secrets from format %d on. "+
			"The ones the platform seeds were made for this tenant when it was provisioned and this restore changes none of them. ", schemaVersion, secretsSchemaVersion)
		encrypted = "Data an app encrypted with a secret the platform generated for it can be read only where that secret is the same as when the bundle was taken: " +
			"in the tenant it was taken of, as long as the app was not purged under random secrets, or on a cluster built from the first one's recovery kit with derived secrets."
	}
	notes := []string{
		credentials +
			"Credentials a person entered -- a repository's password, an SMTP relay's, an API key typed into an app's settings in the vault -- did not come back and have to be entered again.",
		encrypted,
		"Members came back without passwords and have to be sent a reset. App grants, integration bindings and the cache are not in a bundle: grants and bindings are declared state and come from where the tenant is declared.",
	}
	if derivation == gentianov1alpha1.RestoreNamesDerived {
		notes = append(notes, "This bundle's manifest is of format 1, which names apps and kinds only: the artefact names were derived from the tenant the manifest records, "+
			"and the volume claims restored are the ones the apps have now. Databases an app created besides its own are not in a format 1 bundle.")
	}
	return notes
}

// archivedMailboxesPlan is the archived mailboxes of a bundle as they are
// put on record in the tenant restored into: under the name each has in the
// archive, in that tenant's mail domain. A manifest comes from a bundle, and
// a bundle from anywhere: an entry whose name is not an archived mailbox's,
// or whose address has no part before the @ that is a mailbox's, is left
// out -- the mail, if the archive holds any under that name, is refused by
// the Job that puts it back.
func archivedMailboxesPlan(held []bundle.ArchivedMailbox, domain string) []gentianov1alpha1.RestoredArchive {
	var out []gentianov1alpha1.RestoredArchive
	for _, a := range held {
		name, _, ok := cutAddress(a.Address)
		name = strings.ToLower(name)
		if !ok || backup.ValidArchiveName(a.Archive) != nil || backup.ValidMailboxName(name) != nil || !strings.HasPrefix(a.Archive, name+"-") {
			continue
		}
		entry := gentianov1alpha1.RestoredArchive{
			Archive: a.Archive, Domain: domain, Address: name + "@" + domain,
			By: a.By, SizeBytes: a.SizeBytes, Messages: a.Messages,
		}
		if at, err := time.Parse(time.RFC3339, a.ArchivedAt); err == nil {
			when := metav1.NewTime(at)
			entry.ArchivedAt = &when
		}
		if len(entry.By) > 256 {
			entry.By = entry.By[:256]
		}
		out = append(out, entry)
	}
	return out
}
