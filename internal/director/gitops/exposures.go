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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// What a tenant publishes to the internet, as a commit.
//
// A profile DECLARES that it could publish something — Nextcloud's shared
// links, its CalDAV endpoints — and that publishes nothing. A perimeter
// approver ENABLES it, for a host, until a date, and that is this file
// (AD-6). The relation asked is can_expose and never admin, so publishing is
// always its own audit line rather than something an administrator does
// incidentally while doing everything else.
//
// A review date is required and an expiry is not. A surface meant to stay may
// have no end, but none goes unlooked-at: the date is when its owner and the
// approver see it again, and an overdue one is reported.
//
// This file is also the REGISTRY. Every URL a tenant has ever published is
// here with who published it and until when, and a perimeter approver reads
// it back through the director — which is the answer to "what of ours is on
// the internet", asked of the thing that put it there rather than of a scan.

// ExposuresFile is the patch a tenant's published surfaces are written to.
const ExposuresFile = "exposures.yaml"

// Exposure is one published surface, shaped like the CRD's own entry.
type Exposure struct {
	// Install is the Component this publishes from.
	Install string `json:"install"`
	// ExposureName is the entry of that component's profile, whose surface is
	// perimeter.
	ExposureName string `json:"exposureName"`
	// Owner is the subject that enabled it, from the caller's token.
	Owner string `json:"owner"`
	// ExpiresAt is when it stops answering, RFC 3339. Empty for a surface
	// meant to stay.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// ReviewAt is when the owner and the approver look at it again, RFC 3339.
	// Always set.
	ReviewAt string `json:"reviewAt"`
	// Reason is why this is public, in the approver's words.
	Reason string `json:"reason,omitempty"`
	// PublishedAt is when it was first published, RFC 3339. Set once.
	PublishedAt string `json:"publishedAt,omitempty"`
	// LastReviewedBy and LastReviewedAt are who last confirmed it should stay
	// public, and when. Publishing is the first review.
	LastReviewedBy string `json:"lastReviewedBy,omitempty"`
	LastReviewedAt string `json:"lastReviewedAt,omitempty"`
	// Apex says this surface is for the cluster's main address, the bare
	// domain: the approver's own word, which the profile's entry must match.
	// One surface holds the main address at a time.
	Apex bool `json:"apex,omitempty"`
	// ApexAcknowledgedBy and ApexAcknowledgedAt are who acknowledged the rule
	// for a website on the main address -- only a site whose scripts the
	// organisation itself controls -- and when. The approver's, from the
	// caller's token, on every publication and every review of an apex
	// entry. Empty on an entry approved before the rule was asked.
	ApexAcknowledgedBy string `json:"apexAcknowledgedBy,omitempty"`
	ApexAcknowledgedAt string `json:"apexAcknowledgedAt,omitempty"`
}

// ErrMainAddressHeld is a second surface asked for the cluster's main
// address while another holds it.
var ErrMainAddressHeld = errors.New("the cluster's main address is already held")

// ErrMainAddressNotAcknowledged is a surface asked for the cluster's main
// address without the approver's acknowledgement of the rule for it.
var ErrMainAddressNotAcknowledged = errors.New(
	"a website on the cluster's main address needs the approver's acknowledgement of the rule for it")

// live reports an entry that has not expired.
func (e Exposure) live(now time.Time) bool {
	if e.ExpiresAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, e.ExpiresAt)
	return err != nil || now.Before(at)
}

// Key is what makes one unique: a component's entry, published once.
func (e Exposure) Key() string { return e.Install + " " + e.ExposureName }

// TenantExposures reads what a tenant publishes, for the console's view of
// its own perimeter.
func (g *GitOps) TenantExposures(ctx context.Context, tenant string) ([]Exposure, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tenantExposures(ctx, tenant)
}

func (g *GitOps) tenantExposures(ctx context.Context, tenant string) ([]Exposure, error) {
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), ExposuresFile))
	if errors.Is(err, os.ErrNotExist) {
		// Nothing published, which is the answer most tenants have and the
		// one they should have until somebody decides otherwise.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Exposures []Exposure `json:"exposures"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Spec.Exposures, nil
}

// PublishExposure records that a perimeter approver enabled one surface, or
// looked again at one already published.
//
// New terms -- a later review date, an expiry, another reason -- REPLACE the
// old entry's, because two enablements of one surface would leave the
// question of which applies, and the answer would decide how long something
// is on the internet. Two things of the old entry are kept: who published it
// and when. Publishing again is a review, and a review is recorded as one --
// the caller becomes the last reviewer, not the owner -- so the registry can
// answer both who put this on the internet and who last said it should stay.
func (g *GitOps) PublishExposure(ctx context.Context, tenant string, e Exposure, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(e.Install) {
		return Result{}, fmt.Errorf("%w: install %q", ErrInvalidName, e.Install)
	}
	if !ValidName(e.ExposureName) {
		return Result{}, fmt.Errorf("%w: exposure %q", ErrInvalidName, e.ExposureName)
	}
	if e.Owner == "" {
		return Result{}, fmt.Errorf("an exposure needs an owner")
	}
	if e.ReviewAt == "" {
		return Result{}, fmt.Errorf("an exposure needs a review date: what is public is looked at again")
	}
	if _, err := time.Parse(time.RFC3339, e.ReviewAt); err != nil {
		return Result{}, fmt.Errorf("reviewAt must be an RFC 3339 timestamp")
	}
	if e.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, e.ExpiresAt); err != nil {
			return Result{}, fmt.Errorf("expiresAt must be an RFC 3339 timestamp")
		}
	}

	if e.Apex && e.ApexAcknowledgedBy == "" {
		// The API asks first and explains; this is the same rule where the
		// entry is written, so no caller of this package can skip it.
		return Result{}, ErrMainAddressNotAcknowledged
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantExposures(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	if e.Apex {
		// One surface holds the main address at a time. Refused here, with
		// the holder's name, rather than committed and left for the operator
		// to ignore: the approver should hear it when they ask.
		for _, h := range have {
			if h.Apex && h.Key() != e.Key() && h.live(time.Now()) {
				return Result{}, fmt.Errorf(
					"%w by %s/%s, published by %s. One surface holds it at a time: withdraw that one first",
					ErrMainAddressHeld, h.Install, h.ExposureName, h.Owner)
			}
		}
	}
	next := make([]Exposure, 0, len(have)+1)
	replaced := false
	now := time.Now().UTC().Format(time.RFC3339)
	e.LastReviewedBy, e.LastReviewedAt = e.Owner, now
	// The acknowledgement is dated here, like the review, and is never carried
	// over from the old entry: what is recorded is always somebody who was
	// told, for this publication or this review. Only an apex entry has one.
	if e.Apex {
		e.ApexAcknowledgedAt = now
	} else {
		e.ApexAcknowledgedBy, e.ApexAcknowledgedAt = "", ""
	}
	verb := "Publish"
	for _, h := range have {
		if h.Key() == e.Key() {
			// The reviewer is whoever is calling; the owner and the date it
			// was first published are the entry's own.
			e.Owner, e.PublishedAt = h.Owner, h.PublishedAt
			next = append(next, e)
			replaced = true
			verb = "Review"
			continue
		}
		next = append(next, h)
	}
	if !replaced {
		e.PublishedAt = now
		next = append(next, e)
	}
	what := e.Install + "/" + e.ExposureName
	if e.Apex {
		what += " on the cluster's main address"
	}
	return g.writeTenantFileLocked(ctx, tenant, ExposuresFile, renderExposures(tenant, next), listPatch,
		fmt.Sprintf("%s %s for tenant %s", verb, what, tenant), meta)
}

// WithdrawExposure takes one down. The operator removes the proxy, the route
// and the policies on its next reconcile, and the URL stops answering.
func (g *GitOps) WithdrawExposure(ctx context.Context, tenant, install, exposure string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantExposures(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	want := Exposure{Install: install, ExposureName: exposure}.Key()
	next := make([]Exposure, 0, len(have))
	for _, h := range have {
		if h.Key() == want {
			continue
		}
		next = append(next, h)
	}
	if len(next) == len(have) {
		return Result{Status: "unchanged"}, nil
	}
	body := ""
	if len(next) > 0 {
		body = renderExposures(tenant, next)
	}
	return g.writeTenantFileLocked(ctx, tenant, ExposuresFile, body, listPatch,
		fmt.Sprintf("Withdraw %s/%s for tenant %s", install, exposure, tenant), meta)
}

// renderExposures writes the patch a reviewer reads in the commit.
func renderExposures(tenant string, exposures []Exposure) string {
	sorted := make([]Exposure, len(exposures))
	copy(sorted, exposures)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key() < sorted[j].Key() })

	var b strings.Builder
	b.WriteString("# Managed by the director: what this tenant publishes to the internet.\n")
	b.WriteString("#\n")
	b.WriteString("# Each entry is a surface a profile declared and a perimeter approver\n")
	b.WriteString("# enabled, for a host, until a date. The operator stands a proxy in the\n")
	b.WriteString("# tenant's DMZ for each one, forwarding only the paths the profile\n")
	b.WriteString("# declared, with no session and no identity of ours attached.\n")
	b.WriteString("#\n")
	b.WriteString("# Removing an entry, or letting it expire, takes the surface down: the\n")
	b.WriteString("# proxy goes and the URL stops answering. The entry stays in the history\n")
	b.WriteString("# either way, because what a tenant once published is the question an\n")
	b.WriteString("# audit asks.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  exposures:\n")
	for _, e := range sorted {
		b.WriteString("    - install: " + e.Install + "\n")
		b.WriteString("      exposureName: " + e.ExposureName + "\n")
		b.WriteString("      owner: " + e.Owner + "\n")
		b.WriteString("      reviewAt: " + e.ReviewAt + "\n")
		if e.Apex {
			b.WriteString("      # For the cluster's main address: the bare domain.\n")
			b.WriteString("      apex: true\n")
		}
		if e.ApexAcknowledgedBy != "" {
			b.WriteString("      # Who acknowledged the rule for the main address (only a site whose\n")
			b.WriteString("      # scripts the organisation itself controls), and when.\n")
			b.WriteString("      apexAcknowledgedBy: " + e.ApexAcknowledgedBy + "\n")
			b.WriteString("      apexAcknowledgedAt: " + e.ApexAcknowledgedAt + "\n")
		}
		if e.ExpiresAt != "" {
			b.WriteString("      expiresAt: " + e.ExpiresAt + "\n")
		}
		if e.Reason != "" {
			b.WriteString("      reason: " + yamlScalar(e.Reason) + "\n")
		}
		if e.PublishedAt != "" {
			b.WriteString("      publishedAt: " + e.PublishedAt + "\n")
		}
		if e.LastReviewedBy != "" {
			b.WriteString("      lastReviewedBy: " + e.LastReviewedBy + "\n")
		}
		if e.LastReviewedAt != "" {
			b.WriteString("      lastReviewedAt: " + e.LastReviewedAt + "\n")
		}
	}
	return b.String()
}
