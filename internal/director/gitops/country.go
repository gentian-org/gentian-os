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
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// A tenant's country, spec.country of its manifest: an ISO 3166-1 alpha-2
// code in upper case, or nothing. Stated when the tenant is created and
// changed afterwards by whoever may set the tenant's policy, the way its
// languages are; a component whose profile names countryKey is told it.

// ErrInvalidCountry is a country that is not an alpha-2 code.
var ErrInvalidCountry = errors.New("a country is an ISO 3166-1 alpha-2 code: two letters, such as CH or DE")

// countryPattern is the CRD's pattern, checked here as well so the refusal
// names the value rather than arriving as a rejected commit.
var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// NormaliseCountry trims a country and puts it in upper case; "" stays "".
// It refuses anything that is then not two letters.
func NormaliseCountry(country string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(country))
	if c == "" {
		return "", nil
	}
	if !countryPattern.MatchString(c) {
		return "", fmt.Errorf("%w, not %q", ErrInvalidCountry, country)
	}
	return c, nil
}

// parseTenantCountry reads spec.country from a tenant manifest's text.
func parseTenantCountry(text string) (string, error) {
	var doc struct {
		Spec struct {
			Country string `json:"country"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("parse tenant manifest: %w", err)
	}
	return doc.Spec.Country, nil
}

// TenantCountry reads a tenant's country back; "" when it names none.
func (g *GitOps) TenantCountry(ctx context.Context, tenant string) (string, error) {
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
	return parseTenantCountry(string(raw))
}

// SetTenantCountry commits a tenant's country; "" removes it.
func (g *GitOps) SetTenantCountry(ctx context.Context, tenant, country string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	want, err := NormaliseCountry(country)
	if err != nil {
		return Result{}, err
	}
	msg := fmt.Sprintf("Set the country of tenant %s to %s", tenant, want)
	if want == "" {
		msg = fmt.Sprintf("Clear the country of tenant %s", tenant)
	}
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantCountry(text)
		if err != nil {
			return text, "", false, err
		}
		if have == want {
			return text, "unchanged", false, nil
		}
		out, err := setTenantCountry(text, want)
		if err != nil {
			return text, "", false, err
		}
		return out, "updated", true, nil
	})
}

// setTenantCountry writes spec.country into a tenant manifest's text, or
// removes it for "", and checks that it reads back as written.
func setTenantCountry(text, country string) (string, error) {
	var block []string
	if country != "" {
		// Quoted: YAML 1.1 reads a bare NO (Norway) as false.
		block = []string{"  country: \"" + country + "\""}
	}
	out, err := setSpecBlock(text, []string{"country"}, block)
	if err != nil {
		return "", err
	}
	back, err := parseTenantCountry(out)
	if err != nil {
		return "", fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if back != country {
		return "", errors.New("the edited manifest does not read back as written")
	}
	return out, nil
}
