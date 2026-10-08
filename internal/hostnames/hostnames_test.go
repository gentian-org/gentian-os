/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/
package hostnames

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// app is a profile as a catalogue serves one and the director materialises
// it: with an origin, and one gateway entry at the label asked for.
func app(name, subDomain string) *gentianov1alpha1.ComponentProfile {
	p := shipped(name, subDomain)
	p.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/main"}
	return p
}

// shipped is a profile as the platform's chart renders one: no origin and no
// bundle.
func shipped(name, subDomain string) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = name
	p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, SubDomain: subDomain}}
	return p
}

var (
	// A tenant with a domain of its own: <tenant>.<cluster> or a custom one.
	ownDomain = Zone{Domain: "acme.k.example"}
	custom    = Zone{Domain: "acme.example"}
	// The user tenant of a single-tenancy cluster.
	onCluster = Zone{Domain: "k.example", OnClusterDomain: true}
)

// The two lists, pinned: a name added to or taken off either is a decision,
// and this is where it is made twice.
func TestTheReservedNamesAreTheseAndNoOthers(t *testing.T) {
	if got, want := Names(PlatformLabels()), "desktop, admin, store, console, platform, id, auth, login, signin, sign-in, sso, account, accounts"; got != want {
		t.Errorf("platform labels = %s\nwant            %s", got, want)
	}
	if got, want := Names(KernelLabels()), "argocd, corp, headlamp, id, imap, llm, mail, mail-egress, platform, www"; got != want {
		t.Errorf("kernel labels = %s\nwant          %s", got, want)
	}
	owners := map[string]string{}
	for _, tier := range [][]Reserved{PlatformLabels(), KernelLabels()} {
		for _, r := range tier {
			if strings.TrimSpace(r.Why) == "" {
				t.Errorf("%s is reserved with no reason", r.Label)
			}
			if r.Label != strings.ToLower(r.Label) {
				t.Errorf("%s is not lower case, and labels are compared in lower case", r.Label)
			}
			if r.Owner != "" {
				owners[r.Label] = r.Owner
			}
		}
	}
	if fmt.Sprint(owners) != "map[admin:admin-console desktop:desktop store:app-store]" {
		t.Errorf("owners = %v", owners)
	}
	for _, r := range KernelLabels() {
		if r.Owner != "" {
			t.Errorf("kernel label %s has an owner: nothing is admitted to the kernel's own addresses", r.Label)
		}
	}
}

// Every name kept in every tenant is refused to an app wherever the tenant
// is: on a domain of its own, on a custom domain, on the cluster's domain.
func TestAnAppMayTakeNoPlatformNameInAnyTenant(t *testing.T) {
	for _, r := range PlatformLabels() {
		for zoneName, zone := range map[string]Zone{"own domain": ownDomain, "custom domain": custom, "cluster domain": onCluster} {
			for _, label := range []string{r.Label, "x." + r.Label, strings.ToUpper(r.Label), " " + r.Label + " "} {
				refusal := Check("thing", app("thing", label), zone)
				if refusal == nil {
					t.Errorf("%s: an app was admitted to %q", zoneName, label)
					continue
				}
				if refusal.Reserved.Label != r.Label || refusal.Exposure != "web" {
					t.Errorf("%s: %q refused as %+v", zoneName, label, refusal)
				}
			}
			// By the component's name, where the entry names no label.
			if Check(r.Label, app(r.Label, ""), zone) == nil {
				t.Errorf("%s: an app named %s, with no subDomain, was admitted", zoneName, r.Label)
			}
			// Whatever it says its trust tier is.
			claims := app("thing", r.Label)
			claims.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
			claims.Spec.DefaultForTenants = true
			if Check("thing", claims, zone) == nil {
				t.Errorf("%s: an app stating trustTier platform was admitted to %s", zoneName, r.Label)
			}
		}
	}
	// What the person reads.
	msg := Check("thing", app("thing", "admin"), custom).Message()
	for _, want := range []string{"admin.acme.example", "the tenant's administration console", "admin-console", "every tenant", "desktop, admin, store, console"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q: %s", want, msg)
		}
	}
	if msg := Check("thing", app("thing", "console"), Zone{}).Message(); !strings.Contains(msg, "console.<the tenant's domain>") || !strings.Contains(msg, "nothing may take it") {
		t.Errorf("the refusal of a name nobody holds: %s", msg)
	}
}

// The platform's own component holds its own name and no other, and is the
// platform's only when no catalogue brought it.
func TestOnlyThePlatformsOwnComponentHoldsItsName(t *testing.T) {
	own := map[string]string{"desktop": "desktop", "admin-console": "admin", "app-store": "store"}
	for profile, label := range own {
		for zoneName, zone := range map[string]Zone{"own domain": ownDomain, "custom domain": custom, "cluster domain": onCluster} {
			if refusal := Check(profile, shipped(profile, label), zone); refusal != nil {
				t.Errorf("%s: %s was refused its own address: %s", zoneName, profile, refusal.Message())
			}
			// Not another's, not a name nobody holds, and nothing below its own.
			for other := range map[string]bool{"desktop": true, "admin": true, "store": true, "console": true, "login": true, "x." + label: true} {
				if other == label {
					continue
				}
				if Check(profile, shipped(profile, other), zone) == nil {
					t.Errorf("%s: %s was admitted to %s", zoneName, profile, other)
				}
			}
			// A profile of that name a catalogue brought is not it: the
			// director records an origin on everything it materialises, and
			// the bundle beside it.
			for _, annotation := range []string{profilebundle.OriginAnnotation, profilebundle.Annotation} {
				p := shipped(profile, label)
				p.Annotations = map[string]string{annotation: "cluster/main"}
				if Check(profile, p, zone) == nil {
					t.Errorf("%s: a %s carrying %s was admitted to %s", zoneName, profile, annotation, label)
				}
			}
		}
		// Another profile the platform ships, or anything else with no
		// origin, is not this label's owner either.
		if Check("concierge", shipped("concierge", label), ownDomain) == nil {
			t.Errorf("a profile that is not %s was admitted to %s", profile, label)
		}
	}
	// Both entries of one component, as the desktop's API and shell are.
	desktop := shipped("desktop", "desktop")
	desktop.Spec.Expose = append(desktop.Spec.Expose, gentianov1alpha1.ExposureSpec{Name: "api", Surface: gentianov1alpha1.SurfaceGateway, SubDomain: "desktop"})
	if refusal := Check("desktop", desktop, ownDomain); refusal != nil {
		t.Errorf("the desktop's two entries: %s", refusal.Message())
	}
	desktop.Spec.Expose[1].SubDomain = "admin"
	if refusal := Check("desktop", desktop, ownDomain); refusal == nil || refusal.Exposure != "api" {
		t.Errorf("the desktop with one entry at admin: %+v", refusal)
	}
}

// The kernel's own hosts are refused where the tenant is at their level, to
// everything, and nowhere else.
func TestTheKernelsNamesAreRefusedOnTheClustersDomainOnly(t *testing.T) {
	everywhere := map[string]bool{}
	for _, r := range PlatformLabels() {
		everywhere[r.Label] = true
	}
	for _, r := range KernelLabels() {
		for _, label := range []string{r.Label, "a.b." + r.Label} {
			for _, p := range []*gentianov1alpha1.ComponentProfile{app("thing", label), shipped("desktop", label)} {
				refusal := Check(p.Name, p, onCluster)
				if refusal == nil || !refusal.Kernel || refusal.Reserved.Label != r.Label {
					t.Errorf("%s by %s on the cluster's domain: %+v", label, p.Name, refusal)
					continue
				}
				for _, want := range []string{label + ".k.example", "tenancy mode is single", "argocd, corp, headlamp"} {
					if !strings.Contains(refusal.Message(), want) {
						t.Errorf("the refusal of %s does not say %q: %s", label, want, refusal.Message())
					}
				}
			}
		}
		for _, zone := range []Zone{ownDomain, custom} {
			refusal := Check("thing", app("thing", r.Label), zone)
			if everywhere[r.Label] {
				if refusal == nil || refusal.Kernel {
					t.Errorf("%s under %s: %+v, want it refused as a name kept in every tenant", r.Label, zone.Domain, refusal)
				}
				continue
			}
			if refusal != nil {
				t.Errorf("%s under %s was refused: %s", r.Label, zone.Domain, refusal.Message())
			}
		}
	}
}

// What is an app's business stays one: the list is the list, with no
// look-alike matching, and operations -- a catalogue component's address --
// is not on it.
func TestNamesThatAreNotReservedAreAdmitted(t *testing.T) {
	for _, label := range []string{
		"cloud", "chat", "files", "docs", "wiki", "erp", "projects", "data", "matrix", "ai-chat", "llm-admin",
		"operations", "api", "identity", "platformer", "desktop1", "desktop-2", "my-admin", "admins", "storefront",
		"logins", "desktop.acme",
	} {
		for _, zone := range []Zone{ownDomain, custom, onCluster} {
			if refusal := Check("thing", app("thing", label), zone); refusal != nil {
				t.Errorf("%s under %s was refused: %s", label, zone.Domain, refusal.Message())
			}
		}
	}
	// www and mail are a tenant's own on its own domain.
	for _, label := range []string{"www", "mail", "imap", "llm"} {
		if refusal := Check("thing", app("thing", label), custom); refusal != nil {
			t.Errorf("%s on a custom domain was refused: %s", label, refusal.Message())
		}
	}
}

// An entry with no label of its own is not asked: an apex entry, and one
// another component serves on that component's host.
func TestEntriesWithNoHostOfTheirOwnAreNotAsked(t *testing.T) {
	addon := app("calendar", "admin")
	addon.Spec.Expose[0].Backend.Component = "nextcloud"
	if refusal := Check("calendar", addon, onCluster); refusal != nil {
		t.Errorf("an entry another component serves was refused: %s", refusal.Message())
	}
	// Naming itself is not naming another.
	addon.Spec.Expose[0].Backend.Component = "calendar"
	if Check("calendar", addon, ownDomain) == nil {
		t.Error("an entry the component serves itself was not asked")
	}
	apex := app("desktop", "")
	apex.Spec.Expose[0].Apex = true
	apex.Spec.Expose[0].Surface = gentianov1alpha1.SurfacePerimeter
	if refusal := Check("desktop", apex, onCluster); refusal != nil {
		t.Errorf("an apex entry was refused by its component's name: %s", refusal.Message())
	}
	// A perimeter entry has a host like any other.
	public := app("thing", "login")
	public.Spec.Expose[0].Surface = gentianov1alpha1.SurfacePerimeter
	if Check("thing", public, ownDomain) == nil {
		t.Error("a perimeter entry at login was admitted")
	}
	if Check("thing", nil, ownDomain) != nil {
		t.Error("no profile, and a refusal")
	}
}

// docsTable is the table docs/design/routing.md carries, row for row.
func docsTable(tier []Reserved, owners bool) []string {
	rows := make([]string, 0, len(tier))
	for _, r := range tier {
		if !owners {
			rows = append(rows, fmt.Sprintf("| `%s` | %s |", r.Label, r.Why))
			continue
		}
		holder := "nothing"
		if r.Owner != "" {
			holder = "`" + r.Owner + "`"
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", r.Label, holder, r.Why))
	}
	return rows
}

// The documents state the list for a person; a name is added in both or in
// neither.
func TestTheDocumentsCarryTheList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "routing.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	section := func(begin, end string) string {
		_, after, ok := strings.Cut(doc, begin)
		if !ok {
			t.Fatalf("routing.md has no %s", begin)
		}
		body, _, ok := strings.Cut(after, end)
		if !ok {
			t.Fatalf("routing.md has no %s", end)
		}
		return body
	}
	for name, c := range map[string]struct {
		tier   []Reserved
		owners bool
	}{"platform": {PlatformLabels(), true}, "kernel": {KernelLabels(), false}} {
		body := section("<!-- reserved-names:"+name+":begin -->", "<!-- reserved-names:"+name+":end -->")
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "| `") {
				got = append(got, strings.TrimSpace(line))
			}
		}
		want := docsTable(c.tier, c.owners)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("routing.md's table of %s names is not the code's list. It should read:\n%s\n\nand reads:\n%s",
				name, strings.Join(want, "\n"), strings.Join(got, "\n"))
		}
	}
	// The other documents name every platform label too.
	for _, file := range []string{"custom-catalogues.md", "app-customization.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range PlatformLabels() {
			if !strings.Contains(string(raw), "`"+r.Label+"`") {
				t.Errorf("docs/%s does not name %s among the address names an app cannot take", file, r.Label)
			}
		}
	}
}
