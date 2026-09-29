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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// A tenant's realm policy, as a commit.
//
// How strong a password has to be, how long a session lasts, what happens
// after repeated failures. The console used to set these through Keycloak's
// admin API with a credential the desktop held; they are declared state now,
// so the thing that changes them is a commit and the thing that applies them
// is the composition that owns the realm. Nothing holds a Keycloak credential
// at any point, and a realm rebuilt from scratch comes back with the policy
// it had, because the policy is in git rather than in the realm.

// SecurityPolicyFile is the patch a tenant's realm policy is written to.
//
// A patch rather than an object of its own: this modifies the Tenant, and
// kustomize applies patches after the components a tenant pulls in, which is
// what makes it the last word. The same reason resource-plan.yaml is one.
const SecurityPolicyFile = "security-policy.yaml"

// SecurityPolicy is what a caller states, shaped like the CRD's own block
// because the director commits it rather than interpreting it.
type SecurityPolicy struct {
	Password   *PasswordPolicy   `json:"password,omitempty"`
	Session    *SessionPolicy    `json:"session,omitempty"`
	BruteForce *BruteForcePolicy `json:"bruteForce,omitempty"`
}

// PasswordPolicy is how strong a password has to be.
type PasswordPolicy struct {
	MinLength           int32 `json:"minLength,omitempty"`
	RequireDigits       bool  `json:"requireDigits,omitempty"`
	RequireLowercase    bool  `json:"requireLowercase,omitempty"`
	RequireUppercase    bool  `json:"requireUppercase,omitempty"`
	RequireSpecialChars bool  `json:"requireSpecialChars,omitempty"`
	HistoryCount        int32 `json:"historyCount,omitempty"`
	MaxAgeDays          int32 `json:"maxAgeDays,omitempty"`
}

// SessionPolicy is how long a sign-in lasts.
type SessionPolicy struct {
	IdleMinutes int32 `json:"idleMinutes,omitempty"`
	MaxHours    int32 `json:"maxHours,omitempty"`
	RememberMe  bool  `json:"rememberMe,omitempty"`
}

// BruteForcePolicy is what happens after repeated failures.
type BruteForcePolicy struct {
	Enabled                bool  `json:"enabled,omitempty"`
	MaxLoginFailures       int32 `json:"maxLoginFailures,omitempty"`
	LockoutDurationSeconds int32 `json:"lockoutDurationSeconds,omitempty"`
}

// SetTenantSecurityPolicy writes a tenant's realm policy and commits it.
func (g *GitOps) SetTenantSecurityPolicy(ctx context.Context, tenant string, policy SecurityPolicy, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	return g.writeTenantFile(ctx, tenant, SecurityPolicyFile, renderSecurityPolicy(tenant, policy), listPatch,
		fmt.Sprintf("Set the security policy for tenant %s", tenant), meta)
}

// TenantSecurityPolicy reads back what a tenant declares, so a screen shows
// the form it is about to change rather than a blank one.
//
// From git, which is the only place it is: nothing here can ask Keycloak what
// the realm currently says, and that is the point — what is declared is what
// the realm will be, because the composition writes it on every reconcile.
func (g *GitOps) TenantSecurityPolicy(ctx context.Context, tenant string) (*SecurityPolicy, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), SecurityPolicyFile))
	if errors.Is(err, os.ErrNotExist) {
		// Nothing declared, which is a real answer: the realm runs on the
		// defaults the composition states.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Security SecurityPolicy `json:"security"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return &doc.Spec.Security, nil
}

// renderSecurityPolicy writes the patch a reviewer reads in the commit.
func renderSecurityPolicy(tenant string, p SecurityPolicy) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: this tenant's realm policy, set in the\n")
	b.WriteString("# administration console by whoever the commit names. The composition\n")
	b.WriteString("# turns it into the Keycloak realm's own fields, so nothing holds a\n")
	b.WriteString("# credential to apply it and a realm rebuilt from scratch comes back\n")
	b.WriteString("# with the policy it had.\n")
	b.WriteString("#\n")
	b.WriteString("# What is left out is not set: the realm keeps the default the\n")
	b.WriteString("# composition states, rather than a zero written over it.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  security:\n")
	if pw := p.Password; pw != nil {
		b.WriteString("    password:\n")
		writeCount(&b, "      minLength", pw.MinLength)
		writeBool(&b, "      requireDigits", pw.RequireDigits)
		writeBool(&b, "      requireLowercase", pw.RequireLowercase)
		writeBool(&b, "      requireUppercase", pw.RequireUppercase)
		writeBool(&b, "      requireSpecialChars", pw.RequireSpecialChars)
		writeCount(&b, "      historyCount", pw.HistoryCount)
		writeCount(&b, "      maxAgeDays", pw.MaxAgeDays)
	}
	if s := p.Session; s != nil {
		b.WriteString("    session:\n")
		writeCount(&b, "      idleMinutes", s.IdleMinutes)
		writeCount(&b, "      maxHours", s.MaxHours)
		writeBool(&b, "      rememberMe", s.RememberMe)
	}
	if bf := p.BruteForce; bf != nil && bf.Enabled {
		b.WriteString("    bruteForce:\n")
		b.WriteString("      enabled: true\n")
		writeCount(&b, "      maxLoginFailures", bf.MaxLoginFailures)
		writeCount(&b, "      lockoutDurationSeconds", bf.LockoutDurationSeconds)
	}
	return b.String()
}

func writeBool(b *strings.Builder, key string, on bool) {
	if !on {
		return
	}
	b.WriteString(key + ": true\n")
}
