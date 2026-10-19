/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UpdateMode is how an installed app moves to newer builds
// (docs/plans/versioning.md §4).
type UpdateMode string

const (
	// UpdateLatest: the platform moves the app to the newest build the
	// cluster's version line admits, inside the window. The default.
	UpdateLatest UpdateMode = "latest"
	// UpdateManual: the cluster's administrator moves it.
	UpdateManual UpdateMode = "manual"
	// UpdateTenant: the tenant's administrator may move it.
	UpdateTenant UpdateMode = "tenant"
)

// ValidUpdateMode reports whether s names a mode.
func ValidUpdateMode(s string) bool {
	switch UpdateMode(s) {
	case UpdateLatest, UpdateManual, UpdateTenant:
		return true
	}
	return false
}

// UpdateWindow is when the platform moves apps in latest mode: a daily
// window in a time zone. Start after End crosses midnight.
type UpdateWindow struct {
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// Open reports whether the window is open at t. A window with no start or no
// end is always open; one whose time zone cannot be loaded is read in UTC.
func (w *UpdateWindow) Open(t time.Time) bool {
	if w == nil || w.Start == "" || w.End == "" {
		return true
	}
	loc := time.UTC
	if w.Timezone != "" {
		if l, err := time.LoadLocation(w.Timezone); err == nil {
			loc = l
		}
	}
	minutes := func(hhmm string) (int, bool) {
		var h, m int
		if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}
	start, ok1 := minutes(w.Start)
	end, ok2 := minutes(w.End)
	if !ok1 || !ok2 {
		return true
	}
	local := t.In(loc)
	now := local.Hour()*60 + local.Minute()
	if start <= end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

// UpdatePolicy is the cluster's policy for moving apps, from the Cluster
// claim's spec.catalogue.updates.
type UpdatePolicy struct {
	// Mode is the mode for every app not named under Apps.
	Mode UpdateMode `json:"mode"`
	// Window is when latest moves run; nil is any time.
	Window *UpdateWindow `json:"window,omitempty"`
	// Apps names the apps whose mode differs from Mode.
	Apps map[string]UpdateMode `json:"apps,omitempty"`
}

// ModeFor is the mode an app moves under for one tenant: the tenant's
// override where the cluster's administrator wrote one, the cluster's answer
// for the app, the cluster's mode otherwise.
func (p UpdatePolicy) ModeFor(profile, tenantOverride string) UpdateMode {
	if ValidUpdateMode(tenantOverride) {
		return UpdateMode(tenantOverride)
	}
	if mode, ok := p.Apps[profile]; ok && ValidUpdateMode(string(mode)) {
		return mode
	}
	if ValidUpdateMode(string(p.Mode)) {
		return p.Mode
	}
	return UpdateLatest
}

// UpdatePolicy reads the cluster's policy from the claim. A claim that
// states none, or no claim, is the default: latest, any time.
func (g *GitOps) UpdatePolicy(ctx context.Context) (UpdatePolicy, error) {
	var claim struct {
		Spec struct {
			Catalogue struct {
				Updates struct {
					Mode   string        `json:"mode"`
					Window *UpdateWindow `json:"window"`
					Apps   []struct {
						Name string `json:"name"`
						Mode string `json:"mode"`
					} `json:"apps"`
				} `json:"updates"`
			} `json:"catalogue"`
		} `json:"spec"`
	}
	found, err := g.readClaimsFile(ctx, "cluster.yaml", &claim)
	if err != nil {
		return UpdatePolicy{}, fmt.Errorf("read the cluster claim's update policy: %w", err)
	}
	out := UpdatePolicy{Mode: UpdateLatest}
	if !found {
		return out, nil
	}
	if mode := strings.TrimSpace(claim.Spec.Catalogue.Updates.Mode); ValidUpdateMode(mode) {
		out.Mode = UpdateMode(mode)
	}
	if w := claim.Spec.Catalogue.Updates.Window; w != nil && (w.Start != "" || w.End != "") {
		out.Window = w
	}
	for _, a := range claim.Spec.Catalogue.Updates.Apps {
		if a.Name != "" && ValidUpdateMode(a.Mode) {
			if out.Apps == nil {
				out.Apps = map[string]UpdateMode{}
			}
			out.Apps[a.Name] = UpdateMode(a.Mode)
		}
	}
	return out, nil
}

// SetAppUpdateMode writes, or with "" removes, the tenant's override of how
// an app moves to newer builds. The cluster administrator's write; the
// route decides who may.
func (g *GitOps) SetAppUpdateMode(ctx context.Context, tenant, profile, mode string, meta Meta) (Result, error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	if mode != "" && !ValidUpdateMode(mode) {
		return Result{}, fmt.Errorf("%w: update mode %q", ErrInvalidName, mode)
	}
	msg := fmt.Sprintf("chore(%s): %s moves to newer builds in %s mode (via %s)", tenant, profile, mode, meta.actor())
	if mode == "" {
		msg = fmt.Sprintf("chore(%s): %s moves to newer builds as the cluster says (via %s)", tenant, profile, meta.actor())
	}
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		lines := strings.Split(text, "\n")
		if _, _, _, installed := appEntryExtent(lines, profile); !installed {
			return "", "", false, ErrNotInstalled
		}
		after := "digest"
		if appEntryHasKey(lines, profile, "catalogue") {
			after = "catalogue"
		}
		var changed bool
		lines, changed = setAppEntryKey(lines, profile, appUpdateModeKey, mode, after)
		if !changed {
			return text, "unchanged", false, nil
		}
		return strings.Join(lines, "\n"), "updated", true, nil
	})
}

// appUpdateModeKey is the key of an app's entry that overrides how it moves
// to newer builds.
const appUpdateModeKey = "updateMode"

// ErrNotInstalled is a write about an app the tenant does not have.
var ErrNotInstalled = errors.New("the app is not installed in this tenant")
