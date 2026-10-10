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
	"strings"

	"sigs.k8s.io/yaml"
)

// A tenant's realm policy, as a commit.
//
// How long a session lasts and what happens after repeated failures. They
// are declared state, so the thing that changes them is a commit and the
// thing that applies them is the composition that owns the realm. Nothing in
// this path holds a Keycloak credential, and a realm rebuilt from scratch
// comes back with the policy it had, because the policy is in git rather
// than in the realm.
//
// How strong a password has to be is not here. It is set in one place, the
// registrar's action on the realm, and this writes no password block: a file
// that carried one loses it the next time the policy is written.

// SecurityPolicyFile is the patch a tenant's realm policy is written to.
//
// A patch rather than an object of its own: this modifies the Tenant, and
// kustomize applies patches after the components a tenant pulls in, which is
// what makes it the last word. The same reason resource-plan.yaml is one.
const SecurityPolicyFile = "security-policy.yaml"

// SecurityPolicy is what a caller states, shaped like the CRD's own block
// because the director commits it rather than interpreting it.
type SecurityPolicy struct {
	Session    *SessionPolicy    `json:"session,omitempty"`
	BruteForce *BruteForcePolicy `json:"bruteForce,omitempty"`
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
