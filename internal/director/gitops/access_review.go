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

// The last review of who holds a role over the cluster
// (spec.platformAccess.accessReview): who looked, when, and by when they look
// again. The commit that records it lists what they were shown; the claim
// keeps the dates, so whether a review is due is read from the claim alone.

// AccessReview is what spec.platformAccess.accessReview holds.
type AccessReview struct {
	ReviewedAt   string `json:"reviewedAt"`
	ReviewedBy   string `json:"reviewedBy"`
	NextReviewAt string `json:"nextReviewAt"`
}

// Due reports whether the next review's date has passed.
func (r *AccessReview) Due(at time.Time) bool {
	if r == nil {
		return true
	}
	next, err := time.Parse(time.RFC3339, r.NextReviewAt)
	return err != nil || !at.Before(next)
}

// LastAccessReview reads the last review back; nil when there has been none.
func (g *GitOps) LastAccessReview(ctx context.Context) (*AccessReview, error) {
	var doc struct {
		Spec struct {
			PlatformAccess struct {
				AccessReview *AccessReview `json:"accessReview"`
			} `json:"platformAccess"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &doc); err != nil {
		return nil, err
	}
	if r := doc.Spec.PlatformAccess.AccessReview; r != nil && r.ReviewedAt != "" {
		return r, nil
	}
	return nil, nil
}

// RecordAccessReview records a review done now, by the person in meta, the
// next one due at next. reviewed are the lines of what they were shown, the
// commit's body.
func (g *GitOps) RecordAccessReview(ctx context.Context, next time.Time, reviewed []string, meta Meta) (Result, error) {
	review := AccessReview{
		ReviewedAt:   now().UTC().Format(time.RFC3339),
		ReviewedBy:   meta.actor(),
		NextReviewAt: next.UTC().Format(time.RFC3339),
	}
	message := fmt.Sprintf("feat(cluster): access to the cluster reviewed, next by %s (via %s)", review.NextReviewAt[:10], meta.actor())
	if len(reviewed) > 0 {
		message += "\n\nReviewed:\n" + strings.Join(reviewed, "\n")
	}
	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		out := text
		for _, f := range [][2]string{{"reviewedAt", review.ReviewedAt}, {"reviewedBy", review.ReviewedBy}, {"nextReviewAt", review.NextReviewAt}} {
			var err error
			out, _, err = setKeyPath(out, "spec", []string{"platformAccess", "accessReview", f[0]}, quotedLeaf(f[1]))
			if errors.Is(err, errNoRoot) {
				return text, "", false, errors.New("the claim has no spec block")
			}
			if err != nil {
				return text, "", false, err
			}
		}
		return out, "updated", true, nil
	})
}
