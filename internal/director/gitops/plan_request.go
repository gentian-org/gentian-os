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
	"strconv"
	"strings"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's request for another resource plan, as a commit.
//
// The tenant's administrator asks; the cluster's administrator answers by
// moving the tenant (SetResourcePlan, which removes the request in the same
// commit) or by declining it. The request is annotations on the Tenant, in a
// patch of its own beside the plan's, because the operator reads the Tenant
// and not git: that is how the request reaches the Resources screen of both.

// PlanRequestFile is the per-tenant patch a request writes, beside
// tenant.yaml and listed under the kustomization's patches.
const PlanRequestFile = "plan-request.yaml"

// PlanRequest is what a tenant's administrator asks for.
type PlanRequest struct {
	Plan   string
	Reason string
	At     time.Time
}

// SetPlanRequest writes the tenant's request, replacing one it had.
func (g *GitOps) SetPlanRequest(ctx context.Context, tenant string, req PlanRequest, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if strings.TrimSpace(req.Plan) == "" {
		return Result{}, fmt.Errorf("%w: plan name is empty", ErrInvalidName)
	}
	return g.writeTenantFile(ctx, tenant, PlanRequestFile, renderPlanRequest(tenant, req, meta), listPatch,
		fmt.Sprintf("Request resource plan %s for tenant %s", req.Plan, tenant), meta)
}

// ClearPlanRequest removes the tenant's request: withdrawn by the tenant's
// administrator or declined by the cluster's.
func (g *GitOps) ClearPlanRequest(ctx context.Context, tenant, message string, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	return g.writeTenantFile(ctx, tenant, PlanRequestFile, "", listPatch, message, meta)
}

// renderPlanRequest produces the strategic-merge patch for one request.
// Every value is quoted: an annotation is a string, and a reason of "42" or
// "yes" left bare would be a number or a boolean to YAML.
func renderPlanRequest(tenant string, req PlanRequest, meta Meta) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: a request of this tenant's administrator for\n")
	b.WriteString("# another resource plan. The cluster's administrator answers it in the\n")
	b.WriteString("# administration console, by moving the tenant or declining.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("  annotations:\n")
	b.WriteString("    " + gentianov1alpha1.PlanRequestAnnotation + ": " + strconv.Quote(strings.TrimSpace(req.Plan)) + "\n")
	if actor := meta.actor(); actor != "" {
		b.WriteString("    " + gentianov1alpha1.PlanRequestByAnnotation + ": " + strconv.Quote(actor) + "\n")
	}
	b.WriteString("    " + gentianov1alpha1.PlanRequestAtAnnotation + ": " + strconv.Quote(req.At.UTC().Format(time.RFC3339)) + "\n")
	if reason := strings.Join(strings.Fields(req.Reason), " "); reason != "" {
		b.WriteString("    " + gentianov1alpha1.PlanRequestReasonAnnotation + ": " + strconv.Quote(reason) + "\n")
	}
	return b.String()
}
