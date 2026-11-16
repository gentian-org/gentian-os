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
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What a tenant's apps may use at the model gateway, spec.llm of its
// manifest: the external providers it opted in to, and its budget. The
// operator makes the tenant's team at the gateway carry it. Every tenant
// may use the models the cluster serves itself; a provider's models only
// once its manifest names the provider here.

// ErrInvalidModelAccess is model access the Tenant's schema would refuse, or
// that names a provider the Cluster claim does not list.
var ErrInvalidModelAccess = errors.New("invalid model access")

var (
	// ErrModelAccessNotDelegated is a tenant's administrator changing the
	// providers of a tenant whose manifest does not say spec.llm.delegated.
	ErrModelAccessNotDelegated = errors.New("changing the providers is not delegated to this tenant")
	// ErrModelAccessClusterOnly is a tenant's administrator changing the
	// budget or spec.llm.delegated, which only the cluster's administrator
	// sets.
	ErrModelAccessClusterOnly = errors.New("the budget and the delegation are the cluster administrator's")
)

// ModelAccessActor is who changes a tenant's model access.
type ModelAccessActor string

const (
	// ModelAccessByCluster is the cluster's administrator: everything.
	ModelAccessByCluster ModelAccessActor = "cluster"
	// ModelAccessByTenant is the tenant's administrator: the providers,
	// while delegated, with the budget and the delegation as they are.
	ModelAccessByTenant ModelAccessActor = "tenant"
)

var (
	providerName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	budgetDuration = regexp.MustCompile(`^[0-9]+(s|m|h|d|w|mo)$`)
)

// normalisedModelAccess sorts and dedupes the providers; nil when the access
// says nothing, which is how it is written: no spec.llm at all.
func normalisedModelAccess(a *gentianov1alpha1.TenantLLM) *gentianov1alpha1.TenantLLM {
	if a == nil {
		return nil
	}
	out := a.DeepCopy()
	sort.Strings(out.Providers)
	out.Providers = slices.Compact(out.Providers)
	if len(out.Providers) == 0 {
		out.Providers = nil
	}
	if b := out.Budget; b != nil && b.MaxBudget == nil && b.BudgetDuration == "" && b.RPMLimit == nil && b.TPMLimit == nil {
		out.Budget = nil
	}
	if out.Providers == nil && out.Budget == nil && !out.Delegated {
		return nil
	}
	return out
}

// validModelAccess holds the access to the Tenant's schema.
func validModelAccess(a *gentianov1alpha1.TenantLLM) error {
	if a == nil {
		return nil
	}
	var problems []string
	if len(a.Providers) > 32 {
		problems = append(problems, "at most 32 providers")
	}
	for _, p := range a.Providers {
		if len(p) > 40 || !providerName.MatchString(p) {
			problems = append(problems, fmt.Sprintf("%q is not a provider's name", p))
		}
	}
	if b := a.Budget; b != nil {
		if b.MaxBudget != nil && *b.MaxBudget < 0 {
			problems = append(problems, "maxBudget is at least 0")
		}
		if b.BudgetDuration != "" && !budgetDuration.MatchString(b.BudgetDuration) {
			problems = append(problems, fmt.Sprintf("budgetDuration %q is not a period such as 30d or 1mo", b.BudgetDuration))
		}
		if b.RPMLimit != nil && *b.RPMLimit < 1 {
			problems = append(problems, "rpmLimit is at least 1")
		}
		if b.TPMLimit != nil && *b.TPMLimit < 1 {
			problems = append(problems, "tpmLimit is at least 1")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidModelAccess, strings.Join(problems, "; "))
	}
	return nil
}

func parseTenantModelAccess(text string) (*gentianov1alpha1.TenantLLM, error) {
	var doc struct {
		Spec struct {
			LLM *gentianov1alpha1.TenantLLM `json:"llm"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse tenant manifest: %w", err)
	}
	return normalisedModelAccess(doc.Spec.LLM), nil
}

// TenantModelAccess reads what a tenant's apps may use at the model gateway;
// nil when its manifest says nothing, which is the models every tenant may
// use, without a cap.
func (g *GitOps) TenantModelAccess(ctx context.Context, tenant string) (*gentianov1alpha1.TenantLLM, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return parseTenantModelAccess(string(raw))
}

// SetTenantModelAccess commits what a tenant's apps may use at the model
// gateway; nil, or access that names nothing, removes spec.llm. A provider
// has to be one the Cluster claim lists. By the tenant's administrator, the
// manifest has to say spec.llm.delegated, and the budget and the delegation
// have to stay as they are; both are read from the manifest the commit
// edits.
func (g *GitOps) SetTenantModelAccess(
	ctx context.Context, tenant string, access *gentianov1alpha1.TenantLLM, by ModelAccessActor, meta Meta,
) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	want := normalisedModelAccess(access)
	if err := validModelAccess(want); err != nil {
		return Result{}, err
	}
	if want != nil && len(want.Providers) > 0 {
		cluster, err := g.ClusterModels(ctx)
		if err != nil {
			return Result{}, err
		}
		for _, p := range want.Providers {
			if !slices.ContainsFunc(cluster.Providers, func(c ModelProvider) bool { return c.Name == p }) {
				return Result{}, fmt.Errorf("%w: the Cluster claim lists no provider %q", ErrInvalidModelAccess, p)
			}
		}
	}
	msg := fmt.Sprintf("feat(%s): set what the tenant's apps may use at the model gateway (via %s)", tenant, meta.actor())
	if want == nil {
		msg = fmt.Sprintf("feat(%s): the tenant's apps use the models every tenant may, without a cap (via %s)", tenant, meta.actor())
	}
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantModelAccess(text)
		if err != nil {
			return text, "", false, err
		}
		if by != ModelAccessByCluster {
			if have == nil || !have.Delegated {
				return text, "", false, ErrModelAccessNotDelegated
			}
			if want == nil || !want.Delegated || !reflect.DeepEqual(have.Budget, want.Budget) {
				return text, "", false, ErrModelAccessClusterOnly
			}
		}
		if reflect.DeepEqual(have, want) {
			return text, "unchanged", false, nil
		}
		out, err := setSpecBlock(text, []string{"llm"}, renderModelAccess(want))
		if err != nil {
			return text, "", false, err
		}
		back, err := parseTenantModelAccess(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited manifest is not valid YAML: %w", err)
		}
		if !reflect.DeepEqual(back, want) {
			return text, "", false, errors.New("the edited manifest does not read back as written")
		}
		return out, "updated", true, nil
	})
}

// renderModelAccess is spec.llm as lines of the manifest; none for nil.
func renderModelAccess(a *gentianov1alpha1.TenantLLM) []string {
	if a == nil {
		return nil
	}
	out := []string{"  llm:"}
	if len(a.Providers) > 0 {
		out = append(out, "    providers:")
		for _, p := range a.Providers {
			out = append(out, "      - "+textScalar(p))
		}
	}
	if b := a.Budget; b != nil {
		out = append(out, "    budget:")
		if b.MaxBudget != nil {
			out = append(out, "      maxBudget: "+strconv.FormatInt(*b.MaxBudget, 10))
		}
		if b.BudgetDuration != "" {
			out = append(out, "      budgetDuration: "+textScalar(b.BudgetDuration))
		}
		if b.RPMLimit != nil {
			out = append(out, "      rpmLimit: "+strconv.FormatInt(*b.RPMLimit, 10))
		}
		if b.TPMLimit != nil {
			out = append(out, "      tpmLimit: "+strconv.FormatInt(*b.TPMLimit, 10))
		}
	}
	if a.Delegated {
		out = append(out, "    delegated: true")
	}
	return out
}
