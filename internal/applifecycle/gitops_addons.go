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

package applifecycle

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SetAddons rewrites the addons list of one installed app in the tenant YAML.
//
// The values are AppProfile names, not app-side ids: the operator resolves those
// through spec.customization.addon when it builds the XTenant, so neither this
// service nor the App Store needs to know that Odoo calls addons modules.
//
// An empty list is a real selection meaning "none", not a no-op. The activation
// script reconciles, so clearing the list disables what was previously enabled.
func (g *GitOps) SetAddons(ctx context.Context, tenant, profile string, addons []string, actor string) (status, file string, changed bool, err error) {
	file, err = g.tenantFile(ctx, tenant)
	if err != nil {
		return "", "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", file, false, err
	}
	text := string(content)
	updated, ok := rewriteAddons(text, profile, addons)
	if !ok {
		return "not_installed", file, false, nil
	}
	if updated == text {
		return "no_change", file, false, nil
	}
	if err := os.WriteFile(file, []byte(updated), 0o644); err != nil {
		return "", file, false, err
	}
	msg := fmt.Sprintf("feat(%s): set addons for %s (via %s)", tenant, profile, actor)
	if err := g.commit(ctx, file, msg); err != nil {
		return "", file, false, err
	}
	return "updated", file, true, nil
}

// rewriteAddons replaces the addons block of the given profile's list entry,
// reporting false when the profile is not installed.
//
// This is a line editor rather than a YAML round-trip because tenant files are
// hand-maintained and carry comments; a load/dump cycle would reflow all of them
// and make every review a full-file diff. It only touches lines belonging to the
// matched list item, so sibling entries and other keys survive untouched.
func rewriteAddons(text, profile string, addons []string) (string, bool) {
	lines := strings.Split(text, "\n")
	start, end, keyIndent, ok := appEntryExtent(lines, profile)
	if !ok {
		return text, false
	}

	addonsKey := regexp.MustCompile(`^ {` + fmt.Sprint(keyIndent) + `}addons:\s*$`)
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
		if addonsKey.MatchString(line) {
			skipping = true
			continue
		}
		kept = append(kept, line)
	}

	pad := strings.Repeat(" ", keyIndent)
	block := make([]string, 0, len(addons)+1)
	if len(addons) > 0 {
		block = append(block, pad+"addons:")
		for _, a := range addons {
			block = append(block, pad+"- "+a)
		}
	}

	out := make([]string, 0, len(lines))
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
