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
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// A tenant's own positions, as commits.
//
// What a position is -- its name, the roles it holds, the positions it
// includes -- is declared state (Tenant spec.positions), written by the
// director as a patch beside the tenant (positions.yaml), reviewed and
// restored with it. The operator writes it into the rights store from
// there. Who holds a position is not declared and is not here.

// PositionsFile is the patch.
const PositionsFile = "positions.yaml"

// ErrInvalidPosition is a position that cannot be declared; its text says
// why and is the caller's to read.
var ErrInvalidPosition = errors.New("invalid position")

// ErrNoSuchPosition is a position the tenant does not declare.
var ErrNoSuchPosition = errors.New("the tenant declares no such position")

type positionsPatch struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Positions []gentianov1alpha1.TenantPosition `json:"positions"`
	} `json:"spec"`
}

// TenantPositions reads a tenant's declared positions back, by name.
func (g *GitOps) TenantPositions(ctx context.Context, tenant string) ([]gentianov1alpha1.TenantPosition, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tenantPositionsLocked(ctx, tenant)
}

func (g *GitOps) tenantPositionsLocked(ctx context.Context, tenant string) ([]gentianov1alpha1.TenantPosition, error) {
	var doc positionsPatch
	if err := g.readTenantPatch(ctx, tenant, PositionsFile, &doc); err != nil {
		return nil, err
	}
	return doc.Spec.Positions, nil
}

// SetTenantPosition declares one position of a tenant, new or changed, and
// leaves the tenant's others as they are. The whole declaration has to hold
// together: a name that is one, no position every tenant has, nothing
// included that the tenant does not have, no circle.
func (g *GitOps) SetTenantPosition(ctx context.Context, tenant string, position gentianov1alpha1.TenantPosition, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	position = cleanPosition(position)
	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantPositionsLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	next := make([]gentianov1alpha1.TenantPosition, 0, len(have)+1)
	for _, p := range have {
		if p.Name != position.Name {
			next = append(next, p)
		}
	}
	next = append(next, position)
	return g.writePositionsLocked(ctx, tenant, next, fmt.Sprintf("Tenant %s: position %s", tenant, position.Name), meta)
}

// RemoveTenantPosition takes one position out of a tenant's declaration.
// One that another position includes is refused: that other would include
// nothing the tenant has.
func (g *GitOps) RemoveTenantPosition(ctx context.Context, tenant, name string, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantPositionsLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	var next []gentianov1alpha1.TenantPosition
	for _, p := range have {
		if p.Name != name {
			next = append(next, p)
		}
	}
	if len(next) == len(have) {
		return Result{}, fmt.Errorf("%w: %s", ErrNoSuchPosition, name)
	}
	return g.writePositionsLocked(ctx, tenant, next, fmt.Sprintf("Tenant %s: position %s removed", tenant, name), meta)
}

// writePositionsLocked checks a tenant's whole declaration and commits it;
// none removes the patch.
func (g *GitOps) writePositionsLocked(ctx context.Context, tenant string, positions []gentianov1alpha1.TenantPosition, message string, meta Meta) (Result, error) {
	sort.Slice(positions, func(i, j int) bool { return positions[i].Name < positions[j].Name })
	specs := make([]authz.PositionSpec, 0, len(positions))
	for _, p := range positions {
		spec := authz.PositionSpec{Name: p.Name, Includes: p.Includes}
		for _, role := range p.Roles {
			spec.Roles = append(spec.Roles, authz.RoleGrant{App: role.App, Role: role.Role})
		}
		specs = append(specs, spec)
	}
	if err := authz.ValidatePositions(tenant, specs); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrInvalidPosition, err.Error())
	}
	body := ""
	if len(positions) > 0 {
		var doc positionsPatch
		doc.APIVersion, doc.Kind = "gentianos.io/v1alpha1", "Tenant"
		doc.Metadata.Name = tenant
		doc.Spec.Positions = positions
		raw, err := yaml.Marshal(doc)
		if err != nil {
			return Result{}, err
		}
		body = "# Managed by the director: this tenant's own positions, beside the four\n" +
			"# every tenant has. What a position is, is declared here; who holds it is\n" +
			"# given in the Admin Console and lives in the rights store.\n" + string(raw)
	}
	return g.writeTenantFileLocked(ctx, tenant, PositionsFile, body, listPatch, message, meta)
}

// cleanPosition is a position as it is written: trimmed, its inclusions and
// roles without repeats and in a fixed order, so that the same declaration
// is the same file.
func cleanPosition(p gentianov1alpha1.TenantPosition) gentianov1alpha1.TenantPosition {
	p.Name = strings.TrimSpace(p.Name)
	p.Title = strings.TrimSpace(p.Title)
	p.Description = strings.TrimSpace(p.Description)
	seen := map[string]bool{}
	var includes []string
	for _, name := range p.Includes {
		if name = strings.TrimSpace(name); name != "" && !seen[name] {
			seen[name] = true
			includes = append(includes, name)
		}
	}
	sort.Strings(includes)
	p.Includes = includes
	held := map[gentianov1alpha1.TenantPositionRole]bool{}
	var roles []gentianov1alpha1.TenantPositionRole
	for _, role := range p.Roles {
		role.App, role.Role = strings.TrimSpace(role.App), strings.TrimSpace(role.Role)
		if !held[role] {
			held[role] = true
			roles = append(roles, role)
		}
	}
	sort.Slice(roles, func(i, j int) bool {
		if roles[i].App != roles[j].App {
			return roles[i].App < roles[j].App
		}
		return roles[i].Role < roles[j].Role
	})
	p.Roles = roles
	return p
}
