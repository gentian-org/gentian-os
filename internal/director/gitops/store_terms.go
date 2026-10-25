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
	"os"
	"regexp"
	"strconv"
	"time"

	"sigs.k8s.io/yaml"
)

// A tenant's acceptance of the App Store's terms (spec.storeTerms): which
// version the store named, and when. The App Store asks once per version
// and records the answer here; the usage statistics report carries it.
// Recorded, not enforced.

// ErrInvalidTermsVersion is a version the Tenant kind would refuse.
var ErrInvalidTermsVersion = errors.New("not a terms version")

var termsVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// StoreTerms is what spec.storeTerms holds.
type StoreTerms struct {
	Version    string `json:"version"`
	AcceptedAt string `json:"acceptedAt"`
}

// TenantStoreTerms reads the acceptance back; nil when there is none.
func (g *GitOps) TenantStoreTerms(ctx context.Context, tenant string) (*StoreTerms, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			StoreTerms *StoreTerms `json:"storeTerms"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Spec.StoreTerms == nil || doc.Spec.StoreTerms.Version == "" {
		return nil, nil
	}
	return doc.Spec.StoreTerms, nil
}

// SetTenantStoreTerms records that the tenant accepts this version of the
// terms, now: one commit, or "unchanged" when that version is the one
// recorded already -- accepting again does not move the time.
func (g *GitOps) SetTenantStoreTerms(ctx context.Context, tenant, version string, now time.Time, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !termsVersionPattern.MatchString(version) {
		return Result{}, fmt.Errorf("%w: %q", ErrInvalidTermsVersion, version)
	}
	at := now.UTC().Format(time.RFC3339)
	msg := fmt.Sprintf("feat(%s): accept the App Store's terms %s (via %s)", tenant, version, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		out, changed, err := setKeyPath(text, "spec", []string{"storeTerms", "version"}, quotedLeaf(version))
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		out, _, err = setKeyPath(out, "spec", []string{"storeTerms", "acceptedAt"}, quotedLeaf(at))
		if err != nil {
			return text, "", false, err
		}
		return out, "updated", true, nil
	})
}

// quotedLeaf is a scalar written quoted whatever it looks like: a version
// such as 1.10 is a string, not a number.
type quotedLeaf string

func (v quotedLeaf) render(indent int, key string) []string {
	return []string{fmt.Sprintf("%*s%s: %s", indent, "", key, strconv.Quote(string(v)))}
}
func (quotedLeaf) extent([]string, int, int) int { return 1 }
func (v quotedLeaf) same(lines []string, at, _ int) bool {
	return scalarOf(lines[at]) == string(v)
}
