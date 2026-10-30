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
)

// The apps the platform places. A placeable profile (ComponentProfile
// spec.placement) is in a tenant's manifest like any app, as an entry of
// spec.apps marked placed: true, so everything that reads the list -- the
// operator, contracts, grants, network paths -- sees it. What differs is
// who writes it. The director's placement worker places and removes it
// (Place, Unplace), and every other write of an app entry refuses one that
// is marked (ErrPlacedApp).

// appPlacedKey is the key of an app's entry that says the platform placed it.
const appPlacedKey = "placed"

// ErrPlacedApp is a change by hand to an app the platform placed.
var ErrPlacedApp = errors.New("the platform places this app")

// placedAppError names the app a write was refused for.
func placedAppError(profile string) error {
	return fmt.Errorf("%w: %s comes with the platform and is changed only by it: it is not installed, uninstalled, "+
		"moved, purged or given add-ons by hand", ErrPlacedApp, profile)
}

// refusePlaced refuses a write to an entry the platform placed.
func refusePlaced(lines []string, profile string) error {
	if appEntryValue(lines, profile, appPlacedKey) == "true" {
		return placedAppError(profile)
	}
	return nil
}

// Place puts a placeable app in a tenant's manifest, marked placed, or marks
// the entry already there. digest and catalogue pin the build where the
// placement fetched one; otherwise the entry names the profile alone and
// runs the profile the cluster has. defaultGrant gives everyone access to
// it, as an install for everyone does.
//
// The platform tenant takes placed apps: it is where the platform's own
// components run.
func (g *GitOps) Place(ctx context.Context, tenant, profile, digest, catalogue string, defaultGrant bool, meta Meta) (Result, error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	if digest != "" && !digestPattern.MatchString(digest) {
		return Result{}, fmt.Errorf("%w: digest %q", ErrInvalidName, digest)
	}
	if catalogue != "" && (digest == "" || !ValidName(catalogue)) {
		return Result{}, fmt.Errorf("%w: catalog %q", ErrInvalidName, catalogue)
	}
	msg := fmt.Sprintf("feat(%s): place %s (via %s)", tenant, profile, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		return placeEntry(text, profile, digest, catalogue, defaultGrant, meta)
	})
}

// placeEntry is Place on a manifest's text.
func placeEntry(text, profile, digest, catalogue string, defaultGrant bool, meta Meta) (string, string, bool, error) {
	lines := strings.Split(text, "\n")
	if _, _, _, present := appEntryExtent(lines, profile); !present {
		out, ok := insertComponentProfile(text, profile, digest, catalogue, defaultGrant)
		if !ok {
			return "", "", false, errors.New("failed to update apps list")
		}
		lines = strings.Split(out, "\n")
		lines, _ = setAppEntryKey(lines, profile, appPlacedKey, "true", "")
		return strings.Join(lines, "\n"), "placed", true, nil
	}
	changed := false
	var marked bool
	lines, marked = setAppEntryKey(lines, profile, appPlacedKey, "true", "")
	changed = changed || marked
	if digest != "" && appEntryValue(lines, profile, "digest") != digest {
		lines, _ = setAppEntryKey(lines, profile, "digest", digest, appPlacedKey)
		lines, _ = setAppEntryKey(lines, profile, "catalogue", catalogue, "digest")
		lines, _ = setAppEntryKey(lines, profile, appMovedAtKey, meta.movedAt(), "catalogue")
		lines, _ = setAppEntryKey(lines, profile, appMovedByKey, meta.mover(), appMovedAtKey)
		changed = true
	}
	if defaultGrant && appEntryValue(lines, profile, appDefaultGrantKey) != "true" {
		lines, _ = setAppEntryKey(lines, profile, appDefaultGrantKey, "true", appPlacedKey)
		changed = true
	}
	if !changed {
		return text, "already_placed", false, nil
	}
	return strings.Join(lines, "\n"), "placed", true, nil
}

// Unplace takes an app the platform placed out of a tenant's manifest. An
// entry that is not marked placed is the tenant's own and stays.
func (g *GitOps) Unplace(ctx context.Context, tenant, profile string, meta Meta) (Result, error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	msg := fmt.Sprintf("feat(%s): remove the placed %s (via %s)", tenant, profile, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		lines := strings.Split(text, "\n")
		if appEntryValue(lines, profile, appPlacedKey) != "true" {
			return text, "not_placed", false, nil
		}
		out, ok := removeAppEntry(text, profile)
		if !ok || out == text {
			return text, "not_placed", false, nil
		}
		return out, "removed", true, nil
	})
}
