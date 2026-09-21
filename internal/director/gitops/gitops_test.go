/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

	res, err := g.Install(context.Background(), "demo", "element", meta("u-ada"))
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

	res, err := g.Install(context.Background(), "demo", "nextcloud", meta("u-ada"))
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
				_, err := g.Install(context.Background(), tenant, fmt.Sprintf("app-%d-%d", w, i), meta(fmt.Sprintf("u-%d", w)))
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
	if _, err := g.Install(context.Background(), "demo", "element", meta("u-ada")); err == nil {
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
	res, err := g.Install(context.Background(), "demo", "jitsi", meta("u-ada"))
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
		if _, err := g.Install(ctx, bad, "element", meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("tenant %q: err = %v", bad, err)
		}
		if _, err := g.Install(ctx, "demo", bad, meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("profile %q: err = %v", bad, err)
		}
		if bad == "Demo" {
			continue // an addon is the app's own module name; case is its business
		}
		if _, err := g.SetAddons(ctx, "demo", "nextcloud", []string{bad}, meta("u")); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("addon %q: err = %v", bad, err)
		}
	}
	if _, err := g.Install(ctx, "absent", "element", meta("u")); !errors.Is(err, gitops.ErrTenantNotFound) {
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
	if _, err := g.Install(context.Background(), "demo", "element", m); err != nil {
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
