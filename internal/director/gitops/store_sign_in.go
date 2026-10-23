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
	"net/url"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// The store's sign-in through the tenant's own realm (Tenant
// spec.store.signIn): the issuer of the store's realm, written in the tenant's
// manifest. The operator makes the client broker-to-store from it.

// ErrInvalidStoreSignIn is an issuer that cannot be a store's realm.
var ErrInvalidStoreSignIn = errors.New("invalid store sign-in")

// ValidStoreIssuer checks an issuer as the CRD would: https://<host>/<path>,
// with no port, query, fragment or trailing slash.
func ValidStoreIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(issuer, " \t\n\"\\") ||
		len(u.Path) < 2 || strings.HasSuffix(issuer, "/") || len(issuer) > 500 {
		return fmt.Errorf("%w: issuer %q is not https://<host>/<path to the realm>", ErrInvalidStoreSignIn, issuer)
	}
	return nil
}

func parseTenantStoreSignIn(text string) (string, error) {
	var doc struct {
		Spec struct {
			Store *struct {
				SignIn *struct {
					Issuer string `json:"issuer"`
				} `json:"signIn"`
			} `json:"store"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("parse tenant manifest: %w", err)
	}
	if doc.Spec.Store == nil || doc.Spec.Store.SignIn == nil {
		return "", nil
	}
	return doc.Spec.Store.SignIn.Issuer, nil
}

// TenantStoreSignIn reads the store realm's issuer, or "" while the sign-in
// is off.
func (g *GitOps) TenantStoreSignIn(ctx context.Context, tenant string) (string, error) {
	if !ValidName(tenant) {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return parseTenantStoreSignIn(string(raw))
}

// SetTenantStoreSignIn turns the sign-in on with the store realm's issuer, or
// off with "".
func (g *GitOps) SetTenantStoreSignIn(ctx context.Context, tenant, issuer string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if issuer != "" {
		if err := ValidStoreIssuer(issuer); err != nil {
			return Result{}, err
		}
	}
	verb := "let the store sign its people in through the tenant's realm"
	if issuer == "" {
		verb = "stop the store signing its people in through the tenant's realm"
	}
	msg := fmt.Sprintf("feat(%s): %s (via %s)", tenant, verb, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantStoreSignIn(text)
		if err != nil {
			return text, "", false, err
		}
		if have == issuer {
			return text, "unchanged", false, nil
		}
		var block []string
		if issuer != "" {
			block = []string{"  store:", "    signIn:", "      issuer: " + strconv.Quote(issuer)}
		}
		out, err := setSpecBlock(text, []string{"store"}, block)
		if err != nil {
			return text, "", false, err
		}
		back, err := parseTenantStoreSignIn(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited manifest is not valid YAML: %w", err)
		}
		if back != issuer {
			return text, "", false, errors.New("the edited manifest does not read back as written")
		}
		return out, "updated", true, nil
	})
}
