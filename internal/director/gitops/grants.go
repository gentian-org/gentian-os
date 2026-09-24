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

package gitops

import (
	"context"
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// What one app may consume from another, as a commit.
//
// A grant is declared state of the plainest kind: it says that this app, in
// this tenant, may use that much of that contract. It is the thing a binding
// is checked against, so it has to be reviewable — "who let the notes app
// read the whole drive, and when" is a question with an answer, and the
// answer is a commit.
//
// One file per app rather than one file of all grants: a grant is edited per
// app, and a file per app means two people changing two apps do not collide
// in the same file.

// AppGrant is what a caller states.
type AppGrant struct {
	Consume        []ConsumeGrant  `json:"consume,omitempty"`
	AllowConsumers []AllowConsumer `json:"allowConsumers,omitempty"`
}

// ConsumeGrant is one contract this app may consume, and how much of it.
type ConsumeGrant struct {
	Contract string   `json:"contract"`
	Granted  []string `json:"granted,omitempty"`
}

// AllowConsumer is one app permitted to consume from this one.
type AllowConsumer struct {
	App      string   `json:"app"`
	Contract string   `json:"contract"`
	Scope    []string `json:"scope,omitempty"`
}

// AppGrantFile is where one app's grant is written.
func AppGrantFile(app string) string { return "grant-" + app + ".yaml" }

// SetAppGrant writes what one app may consume and commits it.
func (g *GitOps) SetAppGrant(ctx context.Context, tenant, app string, grant AppGrant, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(app) {
		return Result{}, fmt.Errorf("%w: app %q", ErrInvalidName, app)
	}
	return g.writeTenantFile(ctx, tenant, AppGrantFile(app), renderAppGrant(tenant, app, grant), listResource,
		fmt.Sprintf("Set what %s may consume in tenant %s", app, tenant), meta)
}

// ClearAppGrant removes one, which withdraws everything it permitted.
func (g *GitOps) ClearAppGrant(ctx context.Context, tenant, app string, meta Meta) (Result, error) {
	if !ValidName(tenant) || !ValidName(app) {
		return Result{}, fmt.Errorf("%w: tenant %q app %q", ErrInvalidName, tenant, app)
	}
	return g.writeTenantFile(ctx, tenant, AppGrantFile(app), "", listResource,
		fmt.Sprintf("Withdraw what %s may consume in tenant %s", app, tenant), meta)
}

// renderAppGrant writes the object a reviewer reads in the commit.
//
// The namespace is written into the file rather than left to a kustomize
// transformer: adding one to the kustomization would move every namespaced
// object a tenant directory ever gains, and this is the only one so far.
func renderAppGrant(tenant, app string, grant AppGrant) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: what " + app + " may consume in this tenant,\n")
	b.WriteString("# and which apps may consume from it. Set in the administration console\n")
	b.WriteString("# by whoever the commit names. A binding asking for more than this\n")
	b.WriteString("# permits is reported on the Integrations screen and does not work.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: AppGrant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + app + "\n")
	b.WriteString("  namespace: " + layout.Tenant(tenant) + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  app: " + app + "\n")
	if len(grant.Consume) > 0 {
		b.WriteString("  consume:\n")
		for _, c := range grant.Consume {
			b.WriteString("  - contract: " + quoteScalar(c.Contract) + "\n")
			writeStringList(&b, "    granted", c.Granted)
		}
	}
	if len(grant.AllowConsumers) > 0 {
		b.WriteString("  allowConsumers:\n")
		for _, a := range grant.AllowConsumers {
			b.WriteString("  - app: " + quoteScalar(a.App) + "\n")
			b.WriteString("    contract: " + quoteScalar(a.Contract) + "\n")
			writeStringList(&b, "    scope", a.Scope)
		}
	}
	return b.String()
}

func writeStringList(b *strings.Builder, key string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(key + ":\n")
	for _, v := range values {
		b.WriteString(strings.Repeat(" ", len(key)-len(strings.TrimLeft(key, " "))) + "  - " + quoteScalar(v) + "\n")
	}
}

// ── What the platform permits to escape its default posture ─────────────────

// PlatformSecurityFile is where the cluster's waiver allowlist is written.
//
// Beside the claims, like the cluster's backup policy, because it is a thing
// the cluster declares about itself. It replaces the copy the operator chart
// used to create from its own values: one object with two writers is a sync
// conflict waiting for the first person who edits the wrong one.
const PlatformSecurityFile = "platform-security-policy.yaml"

// MacWaiver is one permitted escape: this profile, this policy, this scope.
type MacWaiver struct {
	Profile string `json:"profile"`
	Policy  string `json:"policy"`
	Scope   string `json:"scope"`
}

// SetPlatformSecurity writes the cluster's waiver allowlist and commits it.
//
// An allowlist and not a request: a profile asks for a waiver in its own
// catalogue entry, and this says which of those asks the cluster grants. The
// operator intersects the two, so a waiver here for a profile that asks for
// nothing grants nothing, and is harmless rather than dangerous.
func (g *GitOps) SetPlatformSecurity(ctx context.Context, waivers []MacWaiver, meta Meta) (Result, error) {
	for _, w := range waivers {
		if strings.TrimSpace(w.Profile) == "" || strings.TrimSpace(w.Policy) == "" || strings.TrimSpace(w.Scope) == "" {
			return Result{}, fmt.Errorf("%w: a waiver needs a profile, a policy and a scope", ErrInvalidName)
		}
	}
	return g.writeClaimsFile(ctx, PlatformSecurityFile, renderPlatformSecurity(waivers),
		"Set which MAC waivers this cluster permits", meta)
}

// PlatformSecurity reads back what the cluster declares, so a screen shows
// the list it is about to change rather than only the one in force.
func (g *GitOps) PlatformSecurity(ctx context.Context) ([]MacWaiver, error) {
	var doc struct {
		Spec struct {
			AllowedMacWaivers []MacWaiver `json:"allowedMacWaivers"`
		} `json:"spec"`
	}
	found, err := g.readClaimsFile(ctx, PlatformSecurityFile, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc.Spec.AllowedMacWaivers, nil
}

func renderPlatformSecurity(waivers []MacWaiver) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: which MAC waivers this cluster permits, set\n")
	b.WriteString("# in the administration console by whoever the commit names.\n")
	b.WriteString("#\n")
	b.WriteString("# An allowlist, not a request. A profile asks for a waiver in its own\n")
	b.WriteString("# catalogue entry; the operator grants the intersection of the two, so\n")
	b.WriteString("# an entry here for a profile that asks for nothing grants nothing.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: PlatformSecurityPolicy\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: default\n")
	b.WriteString("spec:\n")
	if len(waivers) == 0 {
		b.WriteString("  # Nothing escapes the default posture.\n")
		b.WriteString("  allowedMacWaivers: []\n")
		return b.String()
	}
	b.WriteString("  allowedMacWaivers:\n")
	for _, w := range waivers {
		b.WriteString("  - profile: " + quoteScalar(w.Profile) + "\n")
		b.WriteString("    policy: " + quoteScalar(w.Policy) + "\n")
		b.WriteString("    scope: " + quoteScalar(w.Scope) + "\n")
	}
	return b.String()
}
