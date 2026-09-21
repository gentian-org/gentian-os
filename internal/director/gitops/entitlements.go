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
	"sort"
	"time"

	"sigs.k8s.io/yaml"
)

// entitlementsFile sits beside the tenant's manifest.
const entitlementsFile = "entitlements.yaml"

// Entitlement is the latest fact the store stated about one catalogue entry for
// one tenant. The file holds the latest fact per entry; git holds the history.
type Entitlement struct {
	Coordinate string     `json:"coordinate"`
	Granted    bool       `json:"granted"`
	IssuedAt   time.Time  `json:"issuedAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Seats      int        `json:"seats,omitempty"`
	// GrantID and KeyID say which signed statement this is and which of the
	// store's keys made it, so the fact can be re-verified from the store's
	// own records.
	GrantID string `json:"grantId"`
	KeyID   string `json:"keyId"`
}

// ErrStaleFact is returned for a fact older than the one already recorded. It
// is what keeps a grant, captured and delivered again after the revocation that
// followed it, from bringing the entitlement back.
var ErrStaleFact = errors.New("a newer fact is already recorded")

const entitlementsHeader = "# Written by the director from statements the store signed. Do not edit:\n" +
	"# the authorization store is rebuilt from this file.\n"

type entitlementsDoc struct {
	Entitlements []Entitlement `json:"entitlements"`
}

// RecordEntitlement commits a fact. It reports "recorded", or "unchanged" when
// this very statement is already the recorded one — a redelivery, which the
// caller may safely apply again.
func (g *GitOps) RecordEntitlement(ctx context.Context, tenant string, fact Entitlement, meta Meta) (Result, error) {
	verb := "grant"
	if !fact.Granted {
		verb = "revoke"
	}
	msg := fmt.Sprintf("feat(%s): %s entitlement %s (via %s)", tenant, verb, fact.Coordinate, meta.actor())
	return g.applyTo(ctx, tenant, entitlementsFile, msg, meta, func(text string) (string, string, bool, error) {
		var doc entitlementsDoc
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
			return "", "", false, fmt.Errorf("parse entitlements: %w", err)
		}
		kept := doc.Entitlements[:0]
		for _, e := range doc.Entitlements {
			if e.Coordinate != fact.Coordinate {
				kept = append(kept, e)
				continue
			}
			if e.GrantID == fact.GrantID {
				return text, "unchanged", false, nil
			}
			if !fact.IssuedAt.After(e.IssuedAt) {
				return "", "", false, ErrStaleFact
			}
		}
		doc.Entitlements = append(kept, fact)
		sort.Slice(doc.Entitlements, func(i, j int) bool { return doc.Entitlements[i].Coordinate < doc.Entitlements[j].Coordinate })
		out, err := yaml.Marshal(doc)
		if err != nil {
			return "", "", false, err
		}
		return entitlementsHeader + string(out), "recorded", true, nil
	})
}

// Entitlements returns the recorded facts for a tenant.
func (g *GitOps) Entitlements(ctx context.Context, tenant string) ([]Entitlement, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), entitlementsFile))
	if errors.Is(err, os.ErrNotExist) {
		return []Entitlement{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc entitlementsDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse entitlements: %w", err)
	}
	if doc.Entitlements == nil {
		return []Entitlement{}, nil
	}
	return doc.Entitlements, nil
}
