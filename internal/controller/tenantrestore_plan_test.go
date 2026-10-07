/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

func chartProfile(name, version string, engine gentianov1alpha1.DatabaseEngine, s3 bool) *gentianov1alpha1.ComponentProfile {
	p := storesProfile(name, engine, s3, false)
	p.Spec.Package = gentianov1alpha1.PackageSpec{Chart: &gentianov1alpha1.ChartRef{Name: name, Version: version}}
	return p
}

func planTenant(name string, apps ...string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, a := range apps {
		t.Spec.Apps = append(t.Spec.Apps, gentianov1alpha1.TenantApp{Profile: a})
	}
	return t
}

// liveFrom answers what the tenant has from a table.
func liveFrom(tenant *gentianov1alpha1.Tenant, profiles map[string]*gentianov1alpha1.ComponentProfile, claims map[string][]string) func(string) (liveApp, error) {
	return func(app string) (liveApp, error) {
		now := liveApp{}
		for _, a := range tenant.Spec.Apps {
			if a.Profile == app {
				now.installed = true
			}
		}
		if now.installed {
			now.profile = profiles[app]
			now.claims = claims[app]
		}
		return now, nil
	}
}

// wikiManifest is a format 2 manifest of tenant "old": wiki with a database,
// the databases its role owns, a bucket and a volume; and notes, a second
// app.
func wikiManifest() *backup.Manifest {
	return &backup.Manifest{
		SchemaVersion: 2,
		Tenant:        "old",
		Identity:      &backup.ManifestIdentity{Realm: "old", Path: backup.IdentityArtefact},
		Shell:         &backup.ManifestStore{Kind: bundle.ArtefactPostgres, Name: "old_shell", Path: backup.PostgresArtefact("old_shell")},
		Apps: []backup.ManifestApp{
			{Name: "wiki", ChartVersion: "2.0.0", DatabaseEngine: "postgresql", Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactPostgres, Name: "old_wiki", Path: "postgres/old_wiki.pgc"},
				{Kind: bundle.ArtefactPostgresOwned, Name: "old_wiki", Path: "postgres/old_wiki.owned.tar.gz"},
				{Kind: bundle.ArtefactS3, Name: "old-wiki", Path: "s3/old-wiki.tar.gz"},
				{Kind: bundle.ArtefactVolume, Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Release: "wiki-release"},
			}},
			{Name: "notes", ChartVersion: "1.0.0", Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactVolume, Name: "notes-release-data", Path: "volumes/notes-release-data.tar.gz"},
			}},
		},
	}
}

// The bundle says what is restored and where each artefact is; the tenant
// says where each goes. A bundle brought in under another tenant's name
// keeps its own artefact names and lands in the new tenant's stores.
func TestARestoreGoesByTheBundlesManifest(t *testing.T) {
	// The tenant has wiki, notes, and drive -- which the bundle does not hold.
	tenant := planTenant("demo", "wiki", "notes", "drive")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		"notes": chartProfile("notes", "1.0.0", "", false),
		"drive": chartProfile("drive", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
	}
	claims := map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}, "drive": {"drive-release-data"}}

	plan, err := planRestore(wikiManifest(), tenant, nil, false, liveFrom(tenant, profiles, claims))
	if err != nil {
		t.Fatal(err)
	}
	if plan.derivation != gentianov1alpha1.RestoreNamesFromManifest || plan.schemaVersion != 2 {
		t.Errorf("derivation = %q, format %d", plan.derivation, plan.schemaVersion)
	}
	if len(plan.apps) != 2 || plan.apps[0].name != "wiki" || plan.apps[1].name != "notes" {
		t.Fatalf("apps = %+v, want the bundle's two and never drive", plan.apps)
	}
	if len(plan.notRestored) != 0 {
		t.Fatalf("notRestored = %+v", plan.notRestored)
	}
	want := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactPostgres, Name: "old_wiki", Path: "postgres/old_wiki.pgc", Target: "demo_wiki"},
		{Kind: bundle.ArtefactPostgresOwned, Name: "old_wiki", Path: "postgres/old_wiki.owned.tar.gz", Target: "demo_wiki"},
		{Kind: bundle.ArtefactS3, Name: "old-wiki", Path: "s3/old-wiki.tar.gz", Target: "demo-wiki"},
		{Kind: bundle.ArtefactVolume, Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Release: "wiki-release", Target: "wiki-release-data"},
	}
	if !reflect.DeepEqual(plan.apps[0].artefacts, want) {
		t.Errorf("wiki's artefacts = %+v\nwant %+v", plan.apps[0].artefacts, want)
	}
	// The tenant's own: the realm and the desktop's database, from where
	// the manifest says, into this tenant's.
	wantWide := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactIdentity, Name: "old", Path: "identity/realm.tar.gz", Target: "demo"},
		{Kind: bundle.ArtefactPostgres, Name: "old_shell", Path: "postgres/old_shell.pgc", Target: "demo_shell"},
	}
	if !reflect.DeepEqual(plan.tenantWide, wantWide) {
		t.Errorf("tenant-wide = %+v\nwant %+v", plan.tenantWide, wantWide)
	}
}

// Nothing the bundle holds is dropped without saying so, and an app is
// restored whole or not at all.
func TestWhatARestoreDoesNotPutBackIsNamedWithTheReason(t *testing.T) {
	base := func() (*gentianov1alpha1.Tenant, map[string]*gentianov1alpha1.ComponentProfile, map[string][]string) {
		tenant := planTenant("demo", "wiki", "notes")
		return tenant, map[string]*gentianov1alpha1.ComponentProfile{
			"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
			"notes": chartProfile("notes", "1.0.0", "", false),
		}, map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}
	}
	cases := []struct {
		name   string
		change func(*gentianov1alpha1.Tenant, map[string]*gentianov1alpha1.ComponentProfile, map[string][]string)
		skip   bool
		says   string
	}{
		{"not installed", func(t *gentianov1alpha1.Tenant, _ map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			t.Spec.Apps = t.Spec.Apps[1:]
		}, false, "is not installed in this tenant"},
		{"no profile", func(_ *gentianov1alpha1.Tenant, p map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			delete(p, "wiki")
		}, false, "has no ComponentProfile"},
		{"an older build installed", func(_ *gentianov1alpha1.Tenant, p map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			p["wiki"].Spec.Package.Chart.Version = "1.9.3"
		}, false, "the older version 1.9.3 is installed"},
		{"a version that cannot be compared", func(_ *gentianov1alpha1.Tenant, p map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			p["wiki"].Spec.Package.Chart.Version = "latest"
		}, false, "cannot be compared"},
		{"another engine", func(_ *gentianov1alpha1.Tenant, p map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			p["wiki"] = chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEngineMariaDB, true)
		}, false, "has a PostgreSQL database in the bundle and a mariadb database here"},
		{"no object storage", func(_ *gentianov1alpha1.Tenant, p map[string]*gentianov1alpha1.ComponentProfile, _ map[string][]string) {
			p["wiki"] = chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false)
		}, false, "has a bucket in the bundle and the installed app has no object storage"},
		{"a claim the tenant does not have", func(_ *gentianov1alpha1.Tenant, _ map[string]*gentianov1alpha1.ComponentProfile, c map[string][]string) {
			c["wiki"] = []string{"wiki-release-other"}
		}, false, "has the volume claim wiki-release-data in the bundle, which is not one of the app's claims here (wiki-release-other)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, profiles, claims := base()
			c.change(tenant, profiles, claims)
			plan, err := planRestore(wikiManifest(), tenant, nil, c.skip, liveFrom(tenant, profiles, claims))
			if err != nil {
				t.Fatal(err)
			}
			// notes is restored; wiki is not, as a whole, and it is said.
			if len(plan.apps) != 1 || plan.apps[0].name != "notes" {
				t.Errorf("apps = %+v, want notes alone", plan.apps)
			}
			if len(plan.notRestored) != 1 || plan.notRestored[0].App != "wiki" || !strings.Contains(plan.notRestored[0].Reason, c.says) {
				t.Errorf("notRestored = %+v, want wiki: …%s…", plan.notRestored, c.says)
			}

			// Asked for by name, the same app refuses the whole restore
			// before anything is changed.
			_, err = planRestore(wikiManifest(), tenant, []string{"wiki", "notes"}, c.skip, liveFrom(tenant, profiles, claims))
			var refused *errRestoreRefused
			if !errors.As(err, &refused) || refused.reason != "CannotRestoreAsAsked" || !strings.Contains(err.Error(), c.says) {
				t.Errorf("named in spec.apps: err = %v, want a refusal saying …%s…", err, c.says)
			}
		})
	}
}

// The build that wrote the data against the build installed.
func TestTheBuildRule(t *testing.T) {
	app := backup.ManifestApp{Name: "wiki", ChartVersion: "2.0.0", Digest: "sha256:aaa"}
	cases := []struct {
		installed, digest string
		skip              bool
		restored          bool
		says              string
	}{
		{"2.0.0", "sha256:aaa", false, true, ""},
		{"2.0.0", "", false, true, ""},
		{"2.0.0", "sha256:bbb", false, true, "another build of version 2.0.0"},
		{"2.1.0", "", false, true, "the app upgrades it when it starts"},
		{"1.9.0", "", false, false, "the older version 1.9.0 is installed"},
		{"1.9.0", "", true, true, "restored because spec.skipVersionCheck is set"},
		{"", "", false, false, "cannot be compared"},
		{"", "", true, true, "could not be compared"},
		{"edge", "", false, false, "cannot be compared"},
	}
	for _, c := range cases {
		profile := chartProfile("wiki", c.installed, "", false)
		if c.installed == "" {
			profile.Spec.Package.Chart = nil
		}
		note, why := buildRule(app, profile, c.digest, c.skip)
		if (why == "") != c.restored {
			t.Errorf("installed %q skip=%v: restored = %v (%q), want %v", c.installed, c.skip, why == "", why, c.restored)
		}
		if said := note + why; !strings.Contains(said, c.says) || (c.says == "" && said != "") {
			t.Errorf("installed %q digest %q: said %q, want …%s…", c.installed, c.digest, said, c.says)
		}
	}
}

// What the installed app has and the bundle does not hold is left as it is,
// and the app's entry says so.
func TestAStoreTheBundleDoesNotCoverIsLeftAndSaid(t *testing.T) {
	tenant := planTenant("demo", "notes")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		// notes has since gained a database and a second claim.
		"notes": chartProfile("notes", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false),
	}
	claims := map[string][]string{"notes": {"notes-release-data", "notes-release-index"}}
	m := wikiManifest()
	m.Apps = m.Apps[1:]
	plan, err := planRestore(m, tenant, nil, false, liveFrom(tenant, profiles, claims))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.apps) != 1 || len(plan.apps[0].artefacts) != 1 {
		t.Fatalf("plan = %+v", plan.apps)
	}
	note := plan.apps[0].note
	if !strings.Contains(note, "database") || !strings.Contains(note, "notes-release-index") || !strings.Contains(note, "left as it is") {
		t.Errorf("note = %q", note)
	}
}

// A format 1 manifest names apps and kinds only. It still restores, by the
// names of the tenant it was taken of, and the result says the names were
// derived.
func TestAFormat1BundleRestoresByDerivationAndSaysSo(t *testing.T) {
	old := &backup.Manifest{
		SchemaVersion: 1,
		Tenant:        "old",
		TenantSpec: &gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{
			DatabasePrefix: "o_", S3Prefix: "o-",
		}},
		Identity: &backup.ManifestIdentity{Realm: "old", Path: "identity/realm.tar.gz"},
		Shell:    &backup.ManifestStore{Kind: "postgres", Name: "o_shell", Path: "postgres/o_shell.pgc"},
		Apps: []backup.ManifestApp{
			{Name: "wiki", ChartVersion: "2.0.0", Stores: []backup.ManifestStore{
				{Kind: "postgres", Name: "wiki"}, {Kind: "s3", Name: "wiki"}, {Kind: "volume", Name: "wiki"},
			}},
			// Format 1 listed the tenant-wide captures among the apps.
			{Name: bundle.TenantComponent, Stores: []backup.ManifestStore{{Kind: "identity", Name: bundle.TenantComponent}}},
		},
	}
	tenant := planTenant("demo", "wiki")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
	}
	plan, err := planRestore(old, tenant, nil, false, liveFrom(tenant, profiles, map[string][]string{"wiki": {"wiki-release-data"}}))
	if err != nil {
		t.Fatal(err)
	}
	if plan.derivation != gentianov1alpha1.RestoreNamesDerived || plan.schemaVersion != 1 {
		t.Fatalf("derivation = %q, format %d", plan.derivation, plan.schemaVersion)
	}
	if len(plan.apps) != 1 || len(plan.notRestored) != 0 {
		t.Fatalf("plan = %+v / %+v", plan.apps, plan.notRestored)
	}
	want := []gentianov1alpha1.BundleArtefact{
		{Kind: "postgres", Name: "o_wiki", Path: "postgres/o_wiki.pgc", Target: "demo_wiki"},
		{Kind: "s3", Name: "o-wiki", Path: "s3/o-wiki.tar.gz", Target: "demo-wiki"},
		{Kind: "volume", Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Target: "wiki-release-data"},
	}
	if !reflect.DeepEqual(plan.apps[0].artefacts, want) {
		t.Errorf("artefacts = %+v\nwant %+v", plan.apps[0].artefacts, want)
	}
	notes := strings.Join(restoreLimits(plan.derivation), "\n")
	if !strings.Contains(notes, "format 1") || !strings.Contains(notes, "derived") {
		t.Errorf("the result does not say the names were derived: %s", notes)
	}
	if strings.Contains(strings.Join(restoreLimits(gentianov1alpha1.RestoreNamesFromManifest), "\n"), "format 1") {
		t.Error("a format 2 restore is flagged as derived")
	}
}

// The manifest comes out of a bundle, and a bundle can come from anywhere.
// Its paths reach a shell that holds the object store's admin credential.
func TestAManifestPathThatLeavesTheBundleRefusesTheRestore(t *testing.T) {
	for _, bad := range []string{"../other/secret", "/etc/passwd", "postgres/$(id).pgc", "postgres/a b.pgc", "postgres/x';rm -rf /;'.pgc", "a/../b", ""} {
		m := wikiManifest()
		m.Apps[0].Stores[0].Path = bad
		tenant := planTenant("demo", "wiki", "notes")
		profiles := map[string]*gentianov1alpha1.ComponentProfile{
			"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
			"notes": chartProfile("notes", "1.0.0", "", false),
		}
		_, err := planRestore(m, tenant, nil, false, liveFrom(tenant, profiles, map[string][]string{"wiki": {"wiki-release-data"}, "notes": {"notes-release-data"}}))
		var refused *errRestoreRefused
		if !errors.As(err, &refused) || refused.reason != "BundleUnusable" {
			t.Errorf("path %q: err = %v, want the restore refused", bad, err)
		}
	}
	for _, ref := range []gentianov1alpha1.BundleRef{
		{Bucket: "demo-gentian-backup", Prefix: "nightly$(id)"},
		{Bucket: "demo;x", Prefix: "nightly"},
		{Bucket: "demo-gentian-backup", Prefix: "../other"},
	} {
		if err := validBundleRef(&ref); err == nil {
			t.Errorf("bundle %+v was accepted", ref)
		}
	}
	if err := validBundleRef(&gentianov1alpha1.BundleRef{Bucket: "gentian-imports", Prefix: "20261007-101500-1a2b3c4d"}); err != nil {
		t.Errorf("an upload's own prefix is refused: %v", err)
	}
}

// Nothing restorable is a refusal that names why, not an empty success.
func TestARestoreWithNothingToPutBackIsRefused(t *testing.T) {
	tenant := planTenant("demo")
	_, err := planRestore(wikiManifest(), tenant, nil, false, liveFrom(tenant, nil, nil))
	var refused *errRestoreRefused
	if !errors.As(err, &refused) || refused.reason != "NothingToRestore" ||
		!strings.Contains(err.Error(), "wiki is not installed") || !strings.Contains(err.Error(), "notes is not installed") {
		t.Fatalf("err = %v", err)
	}
	// An app that is not in the bundle, asked for by name.
	tenant = planTenant("demo", "wiki", "drive")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
	}
	_, err = planRestore(wikiManifest(), tenant, []string{"drive"}, false, liveFrom(tenant, profiles, map[string][]string{"wiki": {"wiki-release-data"}}))
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "drive is not in the bundle") {
		t.Fatalf("err = %v", err)
	}
}
