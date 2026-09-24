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
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// What the cluster knows for the console's remaining screens: which apps
// consume which contracts, what the platform permits to escape the default
// posture, and how much customisation the cluster is carrying.
//
// Reads, like the backup and resources reads beside them. Each of these is
// computed from CRs the operator already reconciles, and computing them here
// rather than in a console is what keeps one answer to each question.

// ── Integrations ────────────────────────────────────────────────────────────

// IntegrationOverview is what one tenant's apps consume and provide.
type IntegrationOverview struct {
	Bindings        []IntegrationBindingInfo `json:"bindings"`
	Grants          []AppGrantInfo           `json:"grants"`
	Summary         IntegrationSummary       `json:"summary"`
	EffectiveAccess []EffectiveAccessRow     `json:"effectiveAccess"`
}

// IntegrationBindingInfo is one contract between two apps.
type IntegrationBindingInfo struct {
	Name         string   `json:"name"`
	Contract     string   `json:"contract"`
	Provider     string   `json:"provider"`
	Consumer     string   `json:"consumer"`
	Capabilities []string `json:"capabilities"`
	State        string   `json:"state"`
}

// AppGrantInfo is what one app has been permitted.
type AppGrantInfo struct {
	Name           string              `json:"name"`
	App            string              `json:"app"`
	Consume        []ConsumeGrantInfo  `json:"consume"`
	AllowConsumers []AllowConsumerInfo `json:"allowConsumers"`
	Phase          string              `json:"phase"`
}

// ConsumeGrantInfo is one contract an app may consume, and how much of it.
type ConsumeGrantInfo struct {
	Contract string   `json:"contract"`
	Granted  []string `json:"granted"`
}

// AllowConsumerInfo is one app permitted to consume from this one.
type AllowConsumerInfo struct {
	App      string   `json:"app"`
	Contract string   `json:"contract"`
	Scope    []string `json:"scope"`
}

// IntegrationSummary counts what the screen leads with.
type IntegrationSummary struct {
	BindingCount    int `json:"bindingCount"`
	GrantCount      int `json:"grantCount"`
	GrantReadyCount int `json:"grantReadyCount"`
}

// EffectiveAccessRow is the join a person actually wants: for one contract
// between two apps, what the binding asks for and what the grant permits.
//
// Computed here rather than left to the screen. A binding that asks for more
// than its grant allows is the interesting row, and spotting it means holding
// both objects at once — which is the operator's job, not a renderer's.
type EffectiveAccessRow struct {
	Contract            string   `json:"contract"`
	Consumer            string   `json:"consumer"`
	Provider            string   `json:"provider"`
	BindingCapabilities []string `json:"bindingCapabilities"`
	GrantedCapabilities []string `json:"grantedCapabilities"`
	GrantPhase          string   `json:"grantPhase"`
	// Ungranted are the capabilities the binding asks for and the grant does
	// not permit. Empty is the ordinary case; anything here is a binding that
	// will not do what its author expected.
	Ungranted []string `json:"ungranted"`
}

// Integrations reports one tenant's bindings and grants.
func (s *Service) Integrations(ctx context.Context, tenantName string) (*IntegrationOverview, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	ns := client.InNamespace(layout.Tenant(tenantName))

	var bindings gentianov1alpha1.IntegrationBindingList
	if err := s.client.List(ctx, &bindings, ns); err != nil {
		return nil, fmt.Errorf("list bindings for %s: %w", tenantName, err)
	}
	var grants gentianov1alpha1.AppGrantList
	if err := s.client.List(ctx, &grants, ns); err != nil {
		return nil, fmt.Errorf("list grants for %s: %w", tenantName, err)
	}

	out := &IntegrationOverview{
		Bindings:        []IntegrationBindingInfo{},
		Grants:          []AppGrantInfo{},
		EffectiveAccess: []EffectiveAccessRow{},
	}
	// granted[app][contract] is what that app may consume.
	granted := map[string]map[string][]string{}
	phase := map[string]string{}

	for i := range grants.Items {
		g := &grants.Items[i]
		info := AppGrantInfo{
			Name: g.Name, App: g.Spec.App, Phase: string(g.Status.Phase),
			Consume: []ConsumeGrantInfo{}, AllowConsumers: []AllowConsumerInfo{},
		}
		phase[g.Spec.App] = string(g.Status.Phase)
		for _, c := range g.Spec.Consume {
			info.Consume = append(info.Consume, ConsumeGrantInfo{Contract: c.Contract, Granted: c.Granted})
			if granted[g.Spec.App] == nil {
				granted[g.Spec.App] = map[string][]string{}
			}
			granted[g.Spec.App][c.Contract] = c.Granted
		}
		for _, a := range g.Spec.AllowConsumers {
			info.AllowConsumers = append(info.AllowConsumers, AllowConsumerInfo{App: a.App, Contract: a.Contract, Scope: a.Scope})
		}
		if g.Status.Phase == gentianov1alpha1.AppGrantPhaseReady {
			out.Summary.GrantReadyCount++
		}
		out.Grants = append(out.Grants, info)
	}

	for i := range bindings.Items {
		b := &bindings.Items[i]
		info := IntegrationBindingInfo{
			Name: b.Name, Contract: b.Spec.Contract,
			Provider: b.Spec.Provider.App, Consumer: b.Spec.Consumer.App,
			Capabilities: b.Spec.Capabilities, State: string(b.Status.State),
		}
		if info.Capabilities == nil {
			info.Capabilities = []string{}
		}
		out.Bindings = append(out.Bindings, info)

		allowed := granted[b.Spec.Consumer.App][b.Spec.Contract]
		row := EffectiveAccessRow{
			Contract: b.Spec.Contract, Consumer: b.Spec.Consumer.App, Provider: b.Spec.Provider.App,
			BindingCapabilities: info.Capabilities, GrantedCapabilities: allowed,
			GrantPhase: phase[b.Spec.Consumer.App], Ungranted: []string{},
		}
		if row.GrantedCapabilities == nil {
			row.GrantedCapabilities = []string{}
		}
		permitted := map[string]bool{}
		for _, c := range allowed {
			permitted[c] = true
		}
		for _, want := range info.Capabilities {
			if !permitted[want] {
				row.Ungranted = append(row.Ungranted, want)
			}
		}
		out.EffectiveAccess = append(out.EffectiveAccess, row)
	}

	out.Summary.BindingCount = len(out.Bindings)
	out.Summary.GrantCount = len(out.Grants)
	sort.Slice(out.Bindings, func(i, j int) bool { return out.Bindings[i].Name < out.Bindings[j].Name })
	sort.Slice(out.Grants, func(i, j int) bool { return out.Grants[i].Name < out.Grants[j].Name })
	return out, nil
}

// ── Platform security ───────────────────────────────────────────────────────

// PlatformSecurityResult is what the cluster permits to escape the default
// posture, and what the catalogue asks for.
type PlatformSecurityResult struct {
	AllowedMacWaivers []MacWaiverInfo `json:"allowedMacWaivers"`
	// CatalogueRequests are the waivers profiles ask for, whether or not the
	// cluster permits them. The difference between the two lists is the
	// screen's whole point: a profile asking for something not allowed will
	// be refused at deploy time, and seeing that before installing beats
	// finding out afterwards.
	CatalogueRequests []MacWaiverCatalogueInfo `json:"catalogueRequests"`
}

// MacWaiverInfo is one permitted escape.
type MacWaiverInfo struct {
	Profile string `json:"profile"`
	Policy  string `json:"policy"`
	Scope   string `json:"scope"`
}

// MacWaiverCatalogueInfo is what one profile asks for.
type MacWaiverCatalogueInfo struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"displayName"`
	MacWaivers  []MacWaiverAsk `json:"macWaivers"`
	Allowed     []MacWaiverAsk `json:"allowed"`
	Refused     []MacWaiverAsk `json:"refused"`
}

// MacWaiverAsk is one policy and scope a profile asks to escape.
type MacWaiverAsk struct {
	Policy string `json:"policy"`
	Scope  string `json:"scope"`
}

// PlatformSecurity reports the policy and what the catalogue asks of it.
func (s *Service) PlatformSecurity(ctx context.Context) (*PlatformSecurityResult, error) {
	out := &PlatformSecurityResult{AllowedMacWaivers: []MacWaiverInfo{}, CatalogueRequests: []MacWaiverCatalogueInfo{}}

	var policies gentianov1alpha1.PlatformSecurityPolicyList
	if err := s.client.List(ctx, &policies); err != nil {
		return nil, fmt.Errorf("list platform security policies: %w", err)
	}
	allowed := map[string]bool{}
	for i := range policies.Items {
		for _, w := range policies.Items[i].Spec.AllowedMacWaivers {
			out.AllowedMacWaivers = append(out.AllowedMacWaivers, MacWaiverInfo{Profile: w.Profile, Policy: w.Policy, Scope: w.Scope})
			allowed[w.Profile+"\x1f"+w.Policy+"\x1f"+w.Scope] = true
		}
	}

	var profiles gentianov1alpha1.AppProfileList
	if err := s.client.List(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("list app profiles: %w", err)
	}
	for i := range profiles.Items {
		p := &profiles.Items[i]
		asks := macWaiverAsks(p.Spec.Security)
		if len(asks) == 0 {
			continue
		}
		entry := MacWaiverCatalogueInfo{
			Name: p.Name, DisplayName: p.Spec.DisplayName,
			MacWaivers: asks, Allowed: []MacWaiverAsk{}, Refused: []MacWaiverAsk{},
		}
		for _, ask := range asks {
			if allowed[p.Name+"\x1f"+ask.Policy+"\x1f"+ask.Scope] {
				entry.Allowed = append(entry.Allowed, ask)
			} else {
				entry.Refused = append(entry.Refused, ask)
			}
		}
		out.CatalogueRequests = append(out.CatalogueRequests, entry)
	}
	sort.Slice(out.CatalogueRequests, func(i, j int) bool {
		return out.CatalogueRequests[i].Name < out.CatalogueRequests[j].Name
	})
	return out, nil
}

// ── Customization debt ──────────────────────────────────────────────────────

// CustomizationRecord is one carried change, with what the reconciler
// computed about it.
type CustomizationRecord struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	Summary       string `json:"summary"`
	TargetProfile string `json:"targetProfile"`
	Rung          string `json:"rung"`
	Scope         string `json:"scope"`
	Owner         string `json:"owner"`
	ReviewBy      string `json:"reviewBy"`
	Phase         string `json:"phase"`
	// The four the reconciler decides. Read, not recomputed: it holds the
	// upstream state and the review dates, and a second opinion here would
	// be a different answer on the same question.
	ReviewOverdue        bool `json:"reviewOverdue"`
	UpstreamStale        bool `json:"upstreamStale"`
	TargetVersionDrift   bool `json:"targetVersionDrift"`
	RungAboveRecommended bool `json:"rungAboveRecommended"`
}

// CustomizationDebt is how much the cluster is carrying, and which of it
// wants attention.
type CustomizationDebt struct {
	TotalRecords int `json:"totalRecords"`
	// CarriedDeltas is what is actually being maintained: everything above
	// rung L0, which is the "we changed nothing" rung.
	CarriedDeltas        int                   `json:"carriedDeltas"`
	ByRung               map[string]int        `json:"byRung"`
	ReviewOverdue        []CustomizationRecord `json:"reviewOverdue"`
	UpstreamStale        []CustomizationRecord `json:"upstreamStale"`
	RungAboveRecommended []CustomizationRecord `json:"rungAboveRecommended"`
	Records              []CustomizationRecord `json:"records"`
}

// CustomizationDebtReport lists every carried customisation.
func (s *Service) CustomizationDebtReport(ctx context.Context) (*CustomizationDebt, error) {
	var list gentianov1alpha1.CustomizationList
	if err := s.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list customizations: %w", err)
	}
	out := &CustomizationDebt{
		ByRung:               map[string]int{},
		ReviewOverdue:        []CustomizationRecord{},
		UpstreamStale:        []CustomizationRecord{},
		RungAboveRecommended: []CustomizationRecord{},
		Records:              []CustomizationRecord{},
	}
	for i := range list.Items {
		c := &list.Items[i]
		record := CustomizationRecord{
			Name: c.Name, Namespace: c.Namespace, Summary: c.Spec.Summary,
			TargetProfile: c.Spec.Target.Profile, Rung: string(c.Spec.Rung),
			Scope: string(c.Spec.Scope), Owner: c.Spec.Owner, ReviewBy: c.Spec.ReviewBy,
			Phase:                string(c.Status.Phase),
			ReviewOverdue:        c.Status.ReviewOverdue,
			UpstreamStale:        c.Status.UpstreamStale,
			TargetVersionDrift:   c.Status.TargetVersionDrift,
			RungAboveRecommended: c.Status.RungAboveRecommended,
		}
		out.Records = append(out.Records, record)
		out.ByRung[record.Rung]++
		// L0 is "we changed nothing"; everything above it is a delta somebody
		// maintains. The rung is a free identifier on the CR, so it is
		// compared as one.
		if record.Rung != customizationRungNone {
			out.CarriedDeltas++
		}
		if record.ReviewOverdue {
			out.ReviewOverdue = append(out.ReviewOverdue, record)
		}
		if record.UpstreamStale {
			out.UpstreamStale = append(out.UpstreamStale, record)
		}
		if record.RungAboveRecommended {
			out.RungAboveRecommended = append(out.RungAboveRecommended, record)
		}
	}
	out.TotalRecords = len(out.Records)
	sort.Slice(out.Records, func(i, j int) bool { return out.Records[i].Name < out.Records[j].Name })
	return out, nil
}

// customizationRungNone is the rung that carries no delta.
const customizationRungNone = "L0"

// macWaiverAsks reads what one profile asks to escape.
func macWaiverAsks(security *gentianov1alpha1.SecuritySpec) []MacWaiverAsk {
	if security == nil || len(security.MacWaivers) == 0 {
		return nil
	}
	out := make([]MacWaiverAsk, 0, len(security.MacWaivers))
	for _, w := range security.MacWaivers {
		out = append(out, MacWaiverAsk{Policy: w.Policy, Scope: w.Scope})
	}
	return out
}
