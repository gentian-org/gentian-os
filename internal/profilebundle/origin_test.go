/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"strings"
	"testing"
)

// The origin is written on the profile by the director, with the bundle. It
// is one of the platform's own annotations and the bundle does not state it,
// which is otherwise exactly what a mismatch is -- so it is the one such key
// the check lets be. Any other key of the platform's that the bundle does not
// state is still refused.
func TestTheRecordedOriginDoesNotDisturbTheCheckOfTheBundle(t *testing.T) {
	pinned := Digest([]byte(wiki))
	for _, origin := range []string{ClusterOrigin("gentian"), TenantOrigin("demo", "ours")} {
		p := held(t, wiki)
		p.Annotations[OriginAnnotation] = origin
		if r := Verify(p, pinned); r != nil {
			t.Fatalf("%s: %s: %s", origin, r.Reason, r.Message)
		}
	}
	p := held(t, wiki)
	p.Annotations["gentianos.io/anything-else"] = "x"
	refused(t, "another platform annotation", Verify(p, pinned), ReasonMismatch, "gentianos.io/anything-else")
	p = held(t, wiki)
	p.Labels = map[string]string{OriginAnnotation: "cluster/gentian"}
	refused(t, "the origin as a label", Verify(p, pinned), ReasonMismatch, OriginAnnotation)

	// A bundle that states an origin itself is held to it, like anything
	// else it states: the director refuses to materialise one, and a profile
	// that arrived some other way is not verified with another origin on it.
	stating := strings.Replace(wiki, "  annotations:\n", "  annotations:\n    "+OriginAnnotation+": cluster/gentian\n", 1)
	if !strings.Contains(stating, OriginAnnotation) {
		t.Fatal("the fixture has no annotations to add to")
	}
	p = held(t, stating)
	p.Annotations[OriginAnnotation] = "tenant/demo/ours"
	refused(t, "an origin other than the bundle's", Verify(p, Digest([]byte(stating))), ReasonMismatch, OriginAnnotation)
}

func TestWhoMayUseAProfileFollowsItsOrigin(t *testing.T) {
	for _, c := range []struct {
		origin, tenant string
		usable         bool
	}{
		{"", "demo", true},
		{"cluster/gentian", "demo", true},
		{"tenant/demo/ours", "demo", true},
		{"tenant/demo/ours", "solo", false},
		{"tenant/demo/ours", "", false},
		{"tenant/demo", "demo", false},
		{"tenant//ours", "", false},
		{"tenant/demo/ours/more", "demo", false},
		{"cluster/", "demo", false},
		{"cluster", "demo", false},
		{"demo/ours", "demo", false},
	} {
		if got := UsableBy(c.origin, c.tenant); got != c.usable {
			t.Errorf("UsableBy(%q, %q) = %v", c.origin, c.tenant, got)
		}
	}
	if o, err := ParseOrigin("tenant/demo/ours"); err != nil || o.Tenant != "demo" || o.Source != "ours" {
		t.Fatalf("ParseOrigin = %+v, %v", o, err)
	}
	if o, err := ParseOrigin("cluster/gentian"); err != nil || o.Tenant != "" || o.Source != "gentian" {
		t.Fatalf("ParseOrigin = %+v, %v", o, err)
	}
}
