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
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func grantMeta() gitops.Meta {
	return gitops.Meta{
		Author:    gitops.Person{Name: "Tom", Email: "tom@example.com"},
		Subject:   "u-tom",
		RequestID: "req-grant-1",
		Decision:  "can_approve_privilege tenant:demo",
	}
}

func egressGrant() gitops.PrivilegeGrant {
	return gitops.PrivilegeGrant{
		Install:   "nextcloud",
		Privilege: "egress/smtp-relay",
		Approver:  "u-tom",
		Reason:    "agreed in the security review on the 14th",
	}
}

// An approval is a commit: the patch, and the kustomization that applies it.
func TestGrantingAPrivilegeIsACommit(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	res, err := g.GrantPrivilege(ctx, "demo", egressGrant(), grantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	dir := "clusters/" + dt.Cluster + "/tenants/demo/"
	patch := dt.RemoteFile(t, remote, dir+gitops.PrivilegesFile)
	for _, want := range []string{
		"kind: Tenant", "name: demo", "privileges:",
		"install: nextcloud", "privilege: egress/smtp-relay", "approver: u-tom",
		"approvedAt: ", `reason: "agreed in the security review on the 14th"`,
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	if !strings.Contains(dt.RemoteFile(t, remote, dir+"kustomization.yaml"), gitops.PrivilegesFile) {
		t.Fatalf("the patch is not applied by the kustomization")
	}
	// And the commit says what was approved, for whom.
	subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%s", "main")
	if !strings.Contains(subject, "egress/smtp-relay") || !strings.Contains(subject, "nextcloud") {
		t.Fatalf("commit subject: %s", subject)
	}
}

// Granting what is already granted on the same terms is not a second commit.
// An approver who presses the button twice has not done anything wrong.
func TestReGrantingOnTheSameTermsIsNotACommit(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	grant := egressGrant()
	grant.ApprovedAt = "2026-09-20T10:00:00Z" // fixed, or the timestamp differs
	if _, err := g.GrantPrivilege(ctx, "demo", grant, grantMeta()); err != nil {
		t.Fatal(err)
	}
	head := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	res, err := g.GrantPrivilege(ctx, "demo", grant, grantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Status != "unchanged" {
		t.Fatalf("re-granting = %+v, want unchanged", res)
	}
	if now := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); now != head {
		t.Fatalf("re-granting moved main from %s to %s", head, now)
	}
}

// Two grants for one privilege would leave the question of which applies, so
// new terms replace the old ones rather than appending.
func TestNewTermsReplaceTheGrantRatherThanAddingOne(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.GrantPrivilege(ctx, "demo", egressGrant(), grantMeta()); err != nil {
		t.Fatal(err)
	}
	renewed := egressGrant()
	renewed.Reason = "renewed for another quarter, reviewed again"
	renewed.ExpiresAt = "2027-01-01T00:00:00Z"
	if _, err := g.GrantPrivilege(ctx, "demo", renewed, grantMeta()); err != nil {
		t.Fatal(err)
	}
	patch := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/tenants/demo/"+gitops.PrivilegesFile)
	if n := strings.Count(patch, "privilege: egress/smtp-relay"); n != 1 {
		t.Fatalf("the privilege is granted %d times:\n%s", n, patch)
	}
	if !strings.Contains(patch, "expiresAt: 2027-01-01T00:00:00Z") ||
		!strings.Contains(patch, "renewed for another quarter") {
		t.Fatalf("the new terms did not land:\n%s", patch)
	}
}

// Revoking the last grant removes the file, rather than leaving a patch that
// declares an empty list -- which would have to mean something.
func TestRevokingTheLastGrantRemovesThePatch(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.GrantPrivilege(ctx, "demo", egressGrant(), grantMeta()); err != nil {
		t.Fatal(err)
	}
	second := egressGrant()
	second.Privilege = "egress/webhooks"
	if _, err := g.GrantPrivilege(ctx, "demo", second, grantMeta()); err != nil {
		t.Fatal(err)
	}

	res, err := g.RevokePrivilege(ctx, "demo", "nextcloud", "egress/smtp-relay", grantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("revoking = %+v", res)
	}
	have, err := g.TenantPrivileges(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(have) != 1 || have[0].Privilege != "egress/webhooks" {
		t.Fatalf("after revoking one, git has %+v", have)
	}

	if _, err := g.RevokePrivilege(ctx, "demo", "nextcloud", "egress/webhooks", grantMeta()); err != nil {
		t.Fatal(err)
	}
	if have, err := g.TenantPrivileges(ctx, "demo"); err != nil || len(have) != 0 {
		t.Fatalf("after revoking both, git has %+v (%v)", have, err)
	}
	kustomization := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/tenants/demo/kustomization.yaml")
	if strings.Contains(kustomization, gitops.PrivilegesFile) {
		t.Fatalf("the kustomization still applies a patch that is gone:\n%s", kustomization)
	}
}

// Revoking what was never granted changes nothing and says so.
func TestRevokingWhatWasNeverGrantedIsUnchanged(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)

	res, err := g.RevokePrivilege(context.Background(), "demo", "nextcloud", "egress/none", grantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Status != "unchanged" {
		t.Fatalf("result = %+v", res)
	}
}

// A reason is free text. It goes through YAML and comes back the same, which
// an unquoted scalar would not manage for any of these.
func TestAReasonSurvivesTheRoundTrip(t *testing.T) {
	awkward := []string{
		"colon: and a value, which YAML would read as a mapping",
		`"already quoted" at the start, which is a different thing again`,
		"two lines,\nbecause somebody pressed return",
		"a trailing space and a #hash comment marker ",
		"tabs\tand 'single' quotes",
	}
	for _, reason := range awkward {
		remote := dt.Remote(t, "demo")
		g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
		ctx := context.Background()

		grant := egressGrant()
		grant.Reason = reason
		if _, err := g.GrantPrivilege(ctx, "demo", grant, grantMeta()); err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		have, err := g.TenantPrivileges(ctx, "demo")
		if err != nil {
			t.Fatalf("%q: reading back: %v", reason, err)
		}
		if len(have) != 1 {
			t.Fatalf("%q: read back %d grants", reason, len(have))
		}
		if have[0].Reason != reason {
			t.Errorf("reason round trip:\n got %q\nwant %q", have[0].Reason, reason)
		}
	}
}

// A grant has to name a valid tenant and a valid install, because both become
// path and object names.
func TestGrantRefusesNamesThatAreNotNames(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.GrantPrivilege(ctx, "../etc", egressGrant(), grantMeta()); err == nil {
		t.Fatal("a tenant named ../etc was accepted")
	}
	bad := egressGrant()
	bad.Install = "../../elsewhere"
	if _, err := g.GrantPrivilege(ctx, "demo", bad, grantMeta()); err == nil {
		t.Fatal("an install named ../../elsewhere was accepted")
	}
	short := egressGrant()
	short.Reason = "  "
	if _, err := g.GrantPrivilege(ctx, "demo", short, grantMeta()); err == nil {
		t.Fatal("a grant with no reason was accepted")
	}
}
