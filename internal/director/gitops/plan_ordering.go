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
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// PlanOrderingFile is the cluster's notice on how a tenant orders another
// plan, among its other declarations.
const PlanOrderingFile = "plan-ordering.yaml"

// ErrInvalidPlanOrdering is a notice the cluster would refuse.
var ErrInvalidPlanOrdering = errors.New("invalid plan ordering notice")

var (
	languageCode = regexp.MustCompile(`^[a-z]{2}$`)
	orderingURL  = regexp.MustCompile(`^https://\S+$`)
	orderingMail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	orderingTel  = regexp.MustCompile(`^\+[0-9][0-9 ]{5,24}$`)
)

// CheckPlanOrdering refuses what the PlanOrdering schema would, before
// anything is committed, in words a form can show.
func CheckPlanOrdering(spec *gentianov1alpha1.PlanOrderingSpec) error {
	spec.URL = strings.TrimSpace(spec.URL)
	spec.Email = strings.TrimSpace(spec.Email)
	spec.Phone = strings.TrimSpace(spec.Phone)
	if len(spec.Text) == 0 || len(spec.Text) > 16 {
		return fmt.Errorf("%w: text is given in 1 to 16 languages", ErrInvalidPlanOrdering)
	}
	for lang, text := range spec.Text {
		text = strings.TrimSpace(text)
		if !languageCode.MatchString(lang) {
			return fmt.Errorf("%w: %q is not a two-letter language code", ErrInvalidPlanOrdering, lang)
		}
		if text == "" || len(text) > 512 {
			return fmt.Errorf("%w: the %s text is 1 to 512 characters", ErrInvalidPlanOrdering, lang)
		}
		spec.Text[lang] = text
	}
	actions := 0
	if spec.URL != "" {
		actions++
		if len(spec.URL) > 512 || !orderingURL.MatchString(spec.URL) {
			return fmt.Errorf("%w: url is an https address", ErrInvalidPlanOrdering)
		}
	}
	if spec.Email != "" {
		actions++
		if len(spec.Email) > 254 || !orderingMail.MatchString(spec.Email) {
			return fmt.Errorf("%w: email is an address", ErrInvalidPlanOrdering)
		}
	}
	if spec.Phone != "" {
		actions++
		if !orderingTel.MatchString(spec.Phone) {
			return fmt.Errorf("%w: phone is a number as dialed from abroad, such as +41 44 000 00 00", ErrInvalidPlanOrdering)
		}
	}
	if actions > 1 {
		return fmt.Errorf("%w: at most one of url, email and phone", ErrInvalidPlanOrdering)
	}
	return nil
}

// PlanOrdering reads the notice; false when the cluster states none.
func (g *GitOps) PlanOrdering(ctx context.Context) (*gentianov1alpha1.PlanOrderingSpec, bool, error) {
	var doc gentianov1alpha1.PlanOrdering
	found, err := g.readClaimsFile(ctx, PlanOrderingFile, &doc)
	if err != nil || !found {
		return nil, found, err
	}
	return &doc.Spec, true, nil
}

// SetPlanOrdering commits the notice.
func (g *GitOps) SetPlanOrdering(ctx context.Context, spec gentianov1alpha1.PlanOrderingSpec, meta Meta) (Result, error) {
	if err := CheckPlanOrdering(&spec); err != nil {
		return Result{}, err
	}
	doc := gentianov1alpha1.PlanOrdering{Spec: spec}
	doc.APIVersion = gentianov1alpha1.GroupVersion.String()
	doc.Kind = "PlanOrdering"
	doc.Name = gentianov1alpha1.PlanOrderingName
	body, err := yaml.Marshal(doc)
	if err != nil {
		return Result{}, err
	}
	header := "# Managed by the director: where a tenant orders another resource plan,\n" +
		"# shown on its own screen, set by whoever the commit names.\n"
	return g.writeClaimsFile(ctx, PlanOrderingFile, header+stripEmptyStatus(string(body)), "Set where a tenant orders another resource plan", meta)
}

// ClearPlanOrdering removes the notice; a tenant is then told to ask the
// cluster's administrator.
func (g *GitOps) ClearPlanOrdering(ctx context.Context, meta Meta) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	return g.writeFileLocked(ctx, filepath.Join(g.claimsDir(), PlanOrderingFile), "",
		"Remove the notice on ordering another resource plan", meta)
}
