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
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// validAddon is looser than a DNS label: an addon is the app's own module name
// (Odoo's are snake_case), but it is still spliced into YAML.
var validAddon = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// AddonPin is the build one addon of an app is pinned to, as the app's entry
// records it under addonPins.
type AddonPin struct {
	// Name is the addon's profile name, as the addons list has it.
	Name string `json:"name"`
	// Digest is the build the addon was installed at.
	Digest string `json:"digest"`
	// Catalogue is the catalogue that build was fetched from: with the name,
	// its coordinate.
	Catalogue string `json:"catalogue,omitempty"`
}

// SetAddons rewrites the addons list of one installed app in the tenant YAML.
// It is SetAddonsPinned with no build to pin.
func (g *GitOps) SetAddons(ctx context.Context, tenant, profile string, addons []string, meta Meta) (res Result, err error) {
	return g.SetAddonsPinned(ctx, tenant, profile, addons, nil, meta)
}

// SetAddonsPinned rewrites the addons list of one installed app in the tenant
// YAML, and the builds its addons are pinned to.
//
// The values are ComponentProfile names, not app-side ids: the operator resolves those
// through spec.customization.addon when it builds the XTenant, so neither this
// service nor the App Store needs to know that Odoo calls addons modules.
//
// An empty list is a real selection meaning "none", not a no-op. The activation
// script reconciles, so clearing the list disables what was previously enabled.
//
// pins are the builds this call states, each for an addon in the list, and
// each vouched for by the caller: it fetched the addon's bundle from that
// catalogue's source and saw it hash to the digest. They are written beside
// the list, under addonPins, keyed by name -- the list itself stays a list of
// names, which is what every reader of it walks.
//
// An addon named with no pin is left as the entry has it, which is what a
// bare install does to an app's own pin: one that was active and pinned
// stays pinned, so that changing the selection does not unpin what was not
// touched, and one that is new to the list is activated unpinned. An addon
// that leaves the list takes its pin with it.
func (g *GitOps) SetAddonsPinned(
	ctx context.Context, tenant, profile string, addons []string, pins []AddonPin, meta Meta,
) (res Result, err error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	listed := map[string]bool{}
	for _, ad := range addons {
		if !validAddon.MatchString(ad) {
			return Result{}, fmt.Errorf("%w: addon %q", ErrInvalidName, ad)
		}
		listed[ad] = true
	}
	stated := map[string]AddonPin{}
	said := make([]string, 0, len(pins))
	for _, pin := range pins {
		// A pinned addon is a profile this director materialised, so its
		// name is a profile's and not merely something safe to splice.
		if !ValidName(pin.Name) || !listed[pin.Name] {
			return Result{}, fmt.Errorf("%w: pinned addon %q", ErrInvalidName, pin.Name)
		}
		if !digestPattern.MatchString(pin.Digest) {
			return Result{}, fmt.Errorf("%w: digest %q", ErrInvalidName, pin.Digest)
		}
		if !ValidName(pin.Catalogue) {
			return Result{}, fmt.Errorf("%w: catalogue %q", ErrInvalidName, pin.Catalogue)
		}
		if _, twice := stated[pin.Name]; twice {
			return Result{}, fmt.Errorf("%w: addon %q is pinned twice", ErrInvalidName, pin.Name)
		}
		stated[pin.Name] = pin
		said = append(said, pin.Name+" at "+shortDigest(pin.Digest))
	}
	msg := fmt.Sprintf("feat(%s): set addons for %s (via %s)", tenant, profile, meta.actor())
	if len(said) > 0 {
		// The digest is in the message for the reason an install's is: it is
		// the thing that was checked.
		msg = fmt.Sprintf("feat(%s): set addons for %s, pinning %s (via %s)",
			tenant, profile, strings.Join(said, ", "), meta.actor())
	}
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		// As an install is: the platform tenant takes no add-ons.
		if adoptsAnotherRealm(text, tenant) {
			return "", "", false, ErrPlatformTenant
		}
		had, err := addonsOf(text, profile)
		if err != nil {
			return "", "", false, err
		}
		final := make([]AddonPin, 0, len(addons))
		for _, ad := range addons {
			if pin, ok := stated[ad]; ok {
				final = append(final, pin)
			} else if pin, ok := had[ad]; ok && pin.Digest != "" {
				final = append(final, pin)
			}
		}
		updated, ok := rewriteAddons(text, profile, addons, final)
		if !ok {
			return text, "not_installed", false, nil
		}
		if updated == text {
			return text, "no_change", false, nil
		}
		return updated, "updated", true, nil
	})
}

// addonsOf reads what one app's entry activates now: every addon in its
// list, with the pin it has, if any. An addon with no pin has an empty one.
//
// Read with a YAML parser although the manifest is written by a line editor:
// reading reflows nothing, and the entry may be in any form a person wrote.
func addonsOf(text, profile string) (map[string]AddonPin, error) {
	var doc struct {
		Spec struct {
			Apps []App `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("the tenant's manifest does not parse: %w", err)
	}
	out := map[string]AddonPin{}
	for _, app := range doc.Spec.Apps {
		if app.Profile != profile {
			continue
		}
		for _, ad := range app.Addons {
			out[ad] = AddonPin{Name: ad}
		}
		for _, pin := range app.AddonPins {
			if _, active := out[pin.Name]; active {
				out[pin.Name] = pin
			}
		}
	}
	return out, nil
}

// rewriteAddons replaces the addons block of the given profile's list entry,
// and its addonPins block, reporting false when the profile is not installed.
// It writes exactly the names and pins it is given: both blocks are removed
// in whatever form they are in, and written again at the end of the entry.
//
// This is a line editor rather than a YAML round-trip because tenant files are
// hand-maintained and carry comments; a load/dump cycle would reflow all of them
// and make every review a full-file diff. It only touches lines belonging to the
// matched list item, so sibling entries and other keys survive untouched.
func rewriteAddons(text, profile string, addons []string, pins []AddonPin) (string, bool) {
	lines := strings.Split(text, "\n")
	start, end, keyIndent, ok := appEntryExtent(lines, profile)
	if !ok {
		return text, false
	}

	// Either key, as the head of a block or with its value on the line: a
	// person may have written `addons: []` or `addons: [a, b]`, and leaving
	// that line beside a new block would be a duplicate key.
	ownKey := regexp.MustCompile(`^ {` + fmt.Sprint(keyIndent) + `}(addons|addonPins):\s*(\S.*)?$`)
	kept := make([]string, 0, end-start)
	skipping := false
	for _, line := range lines[start+1 : end] {
		if skipping {
			// Sequence entries under the key are either deeper or a dash at key depth.
			if strings.TrimSpace(line) != "" && indentOf(line) <= keyIndent &&
				!strings.HasPrefix(strings.TrimLeft(line, " "), "-") {
				skipping = false
			} else {
				continue
			}
		}
		if m := ownKey.FindStringSubmatch(line); m != nil {
			// A value on the key's own line is the whole of it, unless it is
			// only a comment after a block's head.
			skipping = m[2] == "" || strings.HasPrefix(m[2], "#")
			continue
		}
		kept = append(kept, line)
	}
	pad := strings.Repeat(" ", keyIndent)
	block := make([]string, 0, len(addons)+1+3*len(pins)+1)
	if len(addons) > 0 {
		block = append(block, pad+"addons:")
		for _, a := range addons {
			block = append(block, pad+"- "+a)
		}
	}
	if len(pins) > 0 {
		block = append(block, pad+"addonPins:")
		for _, pin := range pins {
			block = append(block, pad+"- name: "+pin.Name, pad+"  digest: "+pin.Digest)
			if pin.Catalogue != "" {
				block = append(block, pad+"  catalogue: "+pin.Catalogue)
			}
		}
	}

	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:start+1]...)
	out = append(out, kept...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), true
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// appEntryExtent locates one `- profile: <name>` list item and the lines belonging
// to it, returning [start, end) and the indent its own keys sit at.
//
// A list item owns every following line indented past its dash; the first line at
// or left of that indent starts a sibling entry or a new section. Getting this
// wrong is how an edit leaves a fragment behind — see removeAppEntry.
func appEntryExtent(lines []string, profile string) (start, end, keyIndent int, ok bool) {
	itemRe := regexp.MustCompile(`^(\s*)-\s+profile:\s+` + regexp.QuoteMeta(profile) + `\s*$`)

	start = -1
	for i, line := range lines {
		if m := itemRe.FindStringSubmatch(line); m != nil {
			start = i
			keyIndent = len(m[1]) + 2
			break
		}
	}
	if start < 0 {
		return 0, 0, 0, false
	}

	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if indentOf(lines[i]) < keyIndent {
			end = i
			break
		}
	}
	return start, end, keyIndent, true
}

// removeAppEntry deletes a whole `- profile: <name>` entry, including the keys
// nested under it.
//
// Removing only the `- profile:` line leaves those keys orphaned. Once an entry
// could carry an addons list, uninstalling such an app produced
//
//	apps:
//	  addons:
//	  - odoo-crm-ce
//	- profile: nextcloud-base-ce
//
// a mapping key followed by sequence items, which does not parse — so every
// reconcile after the uninstall failed and the tenant was stuck.
func removeAppEntry(text, profile string) (string, bool) {
	lines := strings.Split(text, "\n")
	start, end, _, ok := appEntryExtent(lines, profile)
	if !ok {
		return text, false
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	return strings.Join(out, "\n"), true
}
