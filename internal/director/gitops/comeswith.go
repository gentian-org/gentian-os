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
	"regexp"
	"sort"
	"strings"
)

// appEntryName matches the first line of an app's entry.
var appEntryName = regexp.MustCompile(`^\s*-\s+profile:\s+(\S+)\s*$`)

// The apps an app comes with. A profile names them (ComponentProfile
// spec.comesWith); installing the app writes an entry for each one the
// tenant does not have, marked cameWith: <the app>, and uninstalling the app
// removes the entries so marked. An entry the tenant already had carries no
// mark and is nobody's to remove but the tenant's.

// appCameWithKey is the key of an app's entry that names the app it was
// installed together with.
const appCameWithKey = "cameWith"

// ErrCameWithApp is an uninstall by hand of an app that came with another.
var ErrCameWithApp = errors.New("the app came with another app")

// refuseCameWith refuses the uninstall of an entry that came with an app.
func refuseCameWith(lines []string, profile string) error {
	if with := appEntryValue(lines, profile, appCameWithKey); with != "" {
		return fmt.Errorf("%w: %s was installed with %s and is removed with it: uninstall %s",
			ErrCameWithApp, profile, with, with)
	}
	return nil
}

// CameWith is one app to install together with another: the build to pin
// it to and the catalog it was fetched from, or neither where the cluster
// already holds its profile.
type CameWith struct {
	Profile, Digest, Catalogue string
}

// InstallWith installs the apps another app comes with, in one commit, each
// as InstallFrom installs an app and marked with the app it came with. An
// app the tenant already has is left exactly as it is, unmarked and at the
// build it was at. Installed lists the ones this call wrote.
//
// One commit for all of them: an app with several is not half installed
// when a push fails, and the request is not as many pushes long as it has
// apps.
func (g *GitOps) InstallWith(
	ctx context.Context, tenant, with string, apps []CameWith, defaultGrant *bool, meta Meta,
) (res Result, installed []string, err error) {
	if !ValidName(with) {
		return Result{}, nil, fmt.Errorf("%w: profile %q", ErrInvalidName, with)
	}
	var names []string
	for _, a := range apps {
		if !ValidName(a.Profile) {
			return Result{}, nil, fmt.Errorf("%w: profile %q", ErrInvalidName, a.Profile)
		}
		if a.Digest != "" && !digestPattern.MatchString(a.Digest) {
			return Result{}, nil, fmt.Errorf("%w: digest %q", ErrInvalidName, a.Digest)
		}
		if a.Catalogue != "" && (a.Digest == "" || !ValidName(a.Catalogue)) {
			return Result{}, nil, fmt.Errorf("%w: catalog %q", ErrInvalidName, a.Catalogue)
		}
		names = append(names, a.Profile)
	}
	forEveryone := defaultGrant != nil && *defaultGrant
	msg := fmt.Sprintf("feat(%s): install %s with %s (via %s)", tenant, strings.Join(names, ", "), with, meta.actor())
	if forEveryone {
		msg = strings.Replace(msg, " (via ", " for everyone (via ", 1)
	}
	res, err = g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		installed = nil
		if adoptsAnotherRealm(text, tenant) {
			return "", "", false, ErrPlatformTenant
		}
		out := text
		for _, a := range apps {
			if _, _, _, present := appEntryExtent(strings.Split(out, "\n"), a.Profile); present {
				continue
			}
			next, ok := insertComponentProfile(out, a.Profile, a.Digest, a.Catalogue, forEveryone)
			if !ok {
				return "", "", false, errors.New("failed to update apps list")
			}
			marked, _ := setAppEntryKey(strings.Split(next, "\n"), a.Profile, appCameWithKey, with, "")
			out = strings.Join(marked, "\n")
			installed = append(installed, a.Profile)
		}
		if len(installed) == 0 {
			return text, "already_installed", false, nil
		}
		return out, "installed", true, nil
	})
	return res, installed, err
}

// UninstallWith removes an app and, in the same commit, the apps that came
// with it. heirs names, for an app that came with it and that another
// installed app comes with too, that other app: such an entry stays and is
// marked with it instead. Removed lists what went besides the app itself.
func (g *GitOps) UninstallWith(
	ctx context.Context, tenant, profile string, heirs map[string]string, meta Meta,
) (res Result, removed []string, err error) {
	if !ValidName(profile) {
		return Result{}, nil, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	msg := fmt.Sprintf("feat(%s): uninstall %s (via %s)", tenant, profile, meta.actor())
	res, err = g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		removed = nil
		lines := strings.Split(text, "\n")
		if err := refusePlaced(lines, profile); err != nil {
			return "", "", false, err
		}
		if err := refuseCameWith(lines, profile); err != nil {
			return "", "", false, err
		}
		out, ok := removeAppEntry(text, profile)
		if !ok {
			out = text
		}
		for _, name := range cameWith(out, profile) {
			if heir := heirs[name]; heir != "" && heir != profile {
				marked, _ := setAppEntryKey(strings.Split(out, "\n"), name, appCameWithKey, heir, "")
				out = strings.Join(marked, "\n")
				continue
			}
			if next, ok := removeAppEntry(out, name); ok && next != out {
				out = next
				removed = append(removed, name)
			}
		}
		if out == text {
			return text, "not_installed", false, nil
		}
		return out, "uninstalled", true, nil
	})
	return res, removed, err
}

// cameWith lists the entries of a manifest's text marked as having come
// with profile, by name.
func cameWith(text, profile string) []string {
	lines := strings.Split(text, "\n")
	var names []string
	for _, line := range lines {
		m := appEntryName.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if name := strings.Trim(m[1], `"'`); appEntryValue(lines, name, appCameWithKey) == profile {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
