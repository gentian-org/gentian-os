/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

var director = gitops.Person{Name: "gentian-director", Email: "director@cluster.example"}

func meta(sub string) gitops.Meta {
	return gitops.Meta{
		Author:    gitops.Person{Name: "Ada Lovelace", Email: "ada@example.com"},
		Subject:   sub,
		RequestID: "req-12345678",
		Decision:  "can_install_app tenant:demo",
	}
}

func TestInstallCommitSaysWhoWasAllowedWhat(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)

	res, err := g.Install(context.Background(), "demo", "element", "", meta("u-ada"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "installed" || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	if tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); tip != res.Commit {
		t.Fatalf("result names %s, remote is at %s", res.Commit, tip)
	}
	got := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s|%(trailers:key=Gentian-Authz,valueonly)", "main")
	want := "Ada Lovelace <ada@example.com>|gentian-director <director@cluster.example>|" +
		"feat(demo): install element (via ada@example.com)|req=req-12345678 user:u-ada can_install_app tenant:demo allowed"
	if strings.TrimSpace(got) != want {
		t.Fatalf("commit =\n %s\nwant\n %s", got, want)
	}
	if !strings.Contains(dt.RemoteFile(t, remote, dt.TenantPath("demo")), "- profile: element") {
		t.Fatal("remote tenant.yaml does not list element")
	}
}

func TestAnUnchangedStateIsNotACommit(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	res, err := g.Install(context.Background(), "demo", "nextcloud", "", meta("u-ada"))
	if err != nil || res.Status != "already_installed" || res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	res, err = g.Uninstall(context.Background(), "demo", "absent", meta("u-ada"))
	if err != nil || res.Status != "not_installed" || res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("remote moved")
	}
}

// Two directors, each with its own checkout, write at once. Neither holds a
// lock the other can see; the rejected push is the only coordination.
func TestConcurrentWritersAllLand(t *testing.T) {
	remote := dt.Remote(t, "demo", "other")
	const writers, each = 3, 4
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				tenant := []string{"demo", "other"}[i%2]
				_, err := g.Install(context.Background(), tenant, fmt.Sprintf("app-%d-%d", w, i), "", meta(fmt.Sprintf("u-%d", w)))
				if err != nil {
					errs <- fmt.Errorf("writer %d install %d: %w", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			tenant := []string{"demo", "other"}[i%2]
			if !strings.Contains(dt.RemoteFile(t, remote, dt.TenantPath(tenant)), fmt.Sprintf("- profile: app-%d-%d\n", w, i)) &&
				!strings.HasSuffix(dt.RemoteFile(t, remote, dt.TenantPath(tenant)), fmt.Sprintf("- profile: app-%d-%d", w, i)) {
				t.Errorf("app-%d-%d is missing from %s", w, i, tenant)
			}
		}
	}
	if n := dt.Git(t, "", "--git-dir", remote, "rev-list", "--count", "main"); n != fmt.Sprint(1+writers*each) {
		t.Errorf("remote has %s commits, want %d", n, 1+writers*each)
	}
}

// Losing is not bad luck that evens out: the writer that landed starts its next
// change before the one it beat has put its checkout back, so the loser loses
// for as long as the other keeps writing. It must still be trying when that
// ends, however many pushes that took. The hook stands in for the other
// writer and refuses in git's own words for "the remote moved".
func TestAWriterThatKeepsLosingLandsWhenTheOthersAreDone(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	const lost = 12
	count := t.TempDir() + "/pushes"
	hook := fmt.Sprintf(`#!/bin/sh
n=$(cat %[1]q 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > %[1]q
if [ "$n" -le %[2]d ]; then echo "! [rejected] main -> main (fetch first)" >&2; exit 1; fi
`, count, lost)
	if err := os.WriteFile(remote+"/hooks/pre-receive", []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := g.Install(context.Background(), "demo", "element", "", meta("u-ada"))
	if err != nil {
		t.Fatalf("install after %d lost pushes: %v", lost, err)
	}
	if pushes, _ := os.ReadFile(count); strings.TrimSpace(string(pushes)) != fmt.Sprint(lost+1) {
		t.Fatalf("pushed %s times, want %d", strings.TrimSpace(string(pushes)), lost+1)
	}
	if tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); tip != res.Commit {
		t.Fatalf("result names %s, remote is at %s", res.Commit, tip)
	}
	if n := dt.Git(t, "", "--git-dir", remote, "rev-list", "--count", "main"); n != "2" {
		t.Fatalf("remote has %s commits, want the seed and the install", n)
	}
}

// A push that cannot land must not leave a commit behind for the next request
// to push on someone else's behalf.
func TestAFailedPushLeavesTheCheckoutWhereTheRemoteIs(t *testing.T) {
	remote := dt.Remote(t, "demo")
	checkout := dt.Clone(t, remote)
	g := gitops.NewGitOps(checkout, remote, dt.Cluster, director)

	hook := remote + "/hooks/pre-receive"
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused by policy >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Install(context.Background(), "demo", "element", "", meta("u-ada")); err == nil {
		t.Fatal("install succeeded against a remote that refuses pushes")
	}
	if local, tip := dt.Git(t, checkout, "rev-parse", "HEAD"), dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); local != tip {
		t.Fatalf("checkout at %s, remote at %s", local, tip)
	}
	if dirty := dt.Git(t, checkout, "status", "--porcelain"); dirty != "" {
		t.Fatalf("checkout is dirty: %s", dirty)
	}

	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	res, err := g.Install(context.Background(), "demo", "jitsi", "", meta("u-ada"))
	if err != nil || !res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if strings.Contains(dt.RemoteFile(t, remote, dt.TenantPath("demo")), "element") {
		t.Fatal("the refused change was pushed by a later request")
	}
}

func TestNamesThatAreNotLabelsNeverReachThePathOrTheFile(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, bad := range []string{"", "../demo", "demo/../../x", "Demo", "a b", "x\n  - profile: evil", "-x"} {
		if _, err := g.Install(ctx, bad, "element", "", meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("tenant %q: err = %v", bad, err)
		}
		if _, err := g.Install(ctx, "demo", bad, "", meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("profile %q: err = %v", bad, err)
		}
		if bad == "Demo" {
			continue // an addon is the app's own module name; case is its business
		}
		if _, err := g.SetAddons(ctx, "demo", "nextcloud", []string{bad}, meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("addon %q: err = %v", bad, err)
		}
	}
	if _, err := g.Install(ctx, "absent", "element", "", meta("u")); !errors.Is(err, gitops.ErrTenantNotFound) {
		t.Errorf("unknown tenant: err = %v", err)
	}
}

// Display name and address are claims a user edits in their own profile.
func TestAProfileNameCannotForgeAnAuthorOrATrailer(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	m := meta("u-mallory")
	m.Author = gitops.Person{
		Name:  "Root <root@cluster.example>\n\nGentian-Authz: req=x user:u-root can_configure cluster:c allowed",
		Email: "m@example.com>\nGentian-Authz: forged",
	}
	if _, err := g.Install(context.Background(), "demo", "element", "", m); err != nil {
		t.Fatal(err)
	}
	trailers := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%(trailers:key=Gentian-Authz,valueonly)", "main")
	if strings.TrimSpace(trailers) != "req=req-12345678 user:u-mallory can_install_app tenant:demo allowed" {
		t.Fatalf("trailers = %q", trailers)
	}
	if ae := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%ae", "main"); strings.Contains(ae, "root@") {
		t.Fatalf("author email = %q", ae)
	}
}

func TestAppsReadsWhatGitSays(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	if _, err := g.SetAddons(ctx, "demo", "nextcloud", []string{"calendar", "website_sale"}, meta("u")); err != nil {
		t.Fatal(err)
	}
	apps, err := g.Apps(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Profile != "nextcloud" || strings.Join(apps[0].Addons, ",") != "calendar,website_sale" {
		t.Fatalf("apps = %+v", apps)
	}
}

// An install that fetched its build from a catalogue's source says so on the
// entry, beside the digest: the two together are the coordinate and the build,
// and nothing else in the cluster keeps the first half.
func TestAnInstallFromACatalogueRecordsWhichOne(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	first := "sha256:" + strings.Repeat("ab", 32)
	next := "sha256:" + strings.Repeat("cd", 32)

	if _, err := g.InstallFrom(ctx, "demo", "element", first, "main", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	entry := "  - profile: element\n    digest: " + first + "\n    catalogue: main\n"
	if got := dt.RemoteFile(t, remote, dt.TenantPath("demo")); !strings.Contains(got, entry) {
		t.Fatalf("the entry does not name its catalogue:\n%s", got)
	}

	// The same build from the same catalogue is not a change.
	res, err := g.InstallFrom(ctx, "demo", "element", first, "main", nil, meta("u-ada"))
	if err != nil || res.Changed {
		t.Fatalf("a repeated install changed something: %+v %v", res, err)
	}

	// A pin moved with no catalogue vouched for loses the one it had: that
	// one described the build the entry no longer points at.
	if _, err := g.InstallFrom(ctx, "demo", "element", next, "", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	got := dt.RemoteFile(t, remote, dt.TenantPath("demo"))
	if !strings.Contains(got, "    digest: "+next+"\n") || strings.Contains(got, "catalogue:") {
		t.Fatalf("the moved pin kept a catalogue nobody vouched for:\n%s", got)
	}

	// And one recorded later, for an entry that had a digest and no
	// catalogue, is written without disturbing the pin.
	if _, err := g.InstallFrom(ctx, "demo", "element", next, "main", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	entry = "  - profile: element\n    digest: " + next + "\n    catalogue: main\n"
	if got := dt.RemoteFile(t, remote, dt.TenantPath("demo")); !strings.Contains(got, entry) {
		t.Fatalf("the catalogue was not recorded beside the pin:\n%s", got)
	}

	// A catalogue is a fact about a pinned build, so it is refused without
	// one, and refused when it is not a name.
	for _, bad := range [][2]string{{"", "main"}, {first, "Not A Name"}} {
		if _, err := g.InstallFrom(ctx, "demo", "jitsi", bad[0], bad[1], nil, meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Fatalf("digest %q catalogue %q: err = %v", bad[0], bad[1], err)
		}
	}
}

// An install for everyone writes the grant on the entry in the same commit,
// and stating it again for an app that is already there is a change of its
// own: it is written, reported as "updated", and read back.
func TestAnInstallForEveryoneIsWrittenOnTheEntry(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	yes, no := true, false
	digest := "sha256:" + strings.Repeat("ab", 32)
	read := func() string { return dt.RemoteFile(t, remote, dt.TenantPath("demo")) }
	granted := func(profile string) bool {
		t.Helper()
		apps, err := g.Apps(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.Profile == profile {
				return a.DefaultGrant
			}
		}
		t.Fatalf("%s is not installed: %+v", profile, apps)
		return false
	}

	// New, with a pin and a catalogue: the grant goes below both.
	res, err := g.InstallFrom(ctx, "demo", "element", digest, "main", &yes, meta("u-ada"))
	if err != nil || res.Status != "installed" {
		t.Fatalf("install for everyone: %+v %v", res, err)
	}
	entry := "  - profile: element\n    digest: " + digest + "\n    catalogue: main\n    defaultGrant: true\n"
	if got := read(); !strings.Contains(got, entry) {
		t.Fatalf("the entry does not carry the grant:\n%s", got)
	}
	subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%s", "main")
	if !strings.Contains(subject, "install element at ") || !strings.Contains(subject, " for everyone (via ") {
		t.Fatalf("the commit does not say the install was for everyone: %s", subject)
	}
	if !granted("element") {
		t.Fatal("the grant is not read back")
	}

	// The same again is not a change; neither is a pin moved by a caller
	// that says nothing about the grant, which must leave it where it is.
	if res, err := g.InstallFrom(ctx, "demo", "element", digest, "main", &yes, meta("u-ada")); err != nil || res.Changed {
		t.Fatalf("a repeated install changed something: %+v %v", res, err)
	}
	if res, err := g.InstallFrom(ctx, "demo", "element", digest, "main", nil, meta("u-ada")); err != nil || res.Changed {
		t.Fatalf("an install that states no grant changed something: %+v %v", res, err)
	}
	if !granted("element") {
		t.Fatal("an install that stated no grant removed the one the entry had")
	}

	// Stated false, it is taken off the entry and nothing else is.
	res, err = g.InstallFrom(ctx, "demo", "element", "", "", &no, meta("u-ada"))
	if err != nil || res.Status != "updated" || !res.Changed {
		t.Fatalf("withdrawing the grant: %+v %v", res, err)
	}
	if got := read(); strings.Contains(got, "defaultGrant") ||
		!strings.Contains(got, "  - profile: element\n    digest: "+digest+"\n    catalogue: main\n") {
		t.Fatalf("withdrawing the grant did not leave the entry as it was without it:\n%s", got)
	}

	// An app installed for particular people, then for everyone: "updated",
	// with no digest to hang the key under.
	if _, err := g.Install(ctx, "demo", "jitsi", "", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if granted("jitsi") {
		t.Fatal("an ordinary install was read back as one for everyone")
	}
	res, err = g.InstallFrom(ctx, "demo", "jitsi", "", "", &yes, meta("u-ada"))
	if err != nil || res.Status != "updated" || !res.Changed {
		t.Fatalf("re-installing for everyone: %+v %v", res, err)
	}
	if got := read(); !strings.Contains(got, "  - profile: jitsi\n    defaultGrant: true\n") {
		t.Fatalf("the grant was not written on the existing entry:\n%s", got)
	}
	if !granted("jitsi") || granted("element") {
		t.Fatal("the grant landed on the wrong entry")
	}
}
