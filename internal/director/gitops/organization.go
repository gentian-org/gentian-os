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
	"net/mail"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	// The director's image has no zone database of its own.
	_ "time/tzdata"

	"golang.org/x/text/currency"
	"golang.org/x/text/language"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Who a tenant is: its display name and spec.organization, in its manifest.
// Its administrators fill them in under Tenant Settings beside the country
// and languages, which keep their own calls; a component whose profile names
// organizationKey is told all of them.

// ErrInvalidOrganization is an organisation the manifest could not hold.
var ErrInvalidOrganization = errors.New("invalid organization")

var (
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	websitePattern  = regexp.MustCompile(`^https?://[^\s]+$`)
)

// Organization is who a tenant is, as Tenant Settings shows it.
type Organization struct {
	DisplayName  string                              `json:"displayName"`
	Country      string                              `json:"country"`
	Locales      []string                            `json:"locales"`
	Organization gentianov1alpha1.TenantOrganization `json:"organization"`
	// Missing names the required facts not given yet
	// (v1alpha1.OrganizationRequired); empty when the tenant is complete.
	Missing []string `json:"missing"`
}

// parseTenantOrganization reads the display name, country and organisation
// from a tenant manifest's text.
func parseTenantOrganization(text string) (gentianov1alpha1.TenantSpec, error) {
	var doc struct {
		Spec struct {
			DisplayName  string                               `json:"displayName"`
			Country      string                               `json:"country"`
			Organization *gentianov1alpha1.TenantOrganization `json:"organization"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return gentianov1alpha1.TenantSpec{}, fmt.Errorf("parse tenant manifest: %w", err)
	}
	return gentianov1alpha1.TenantSpec{
		DisplayName:  doc.Spec.DisplayName,
		Country:      doc.Spec.Country,
		Organization: doc.Spec.Organization,
	}, nil
}

// TenantOrganization reads who a tenant is back, with what is still missing.
func (g *GitOps) TenantOrganization(ctx context.Context, tenant string) (Organization, error) {
	if !ValidName(tenant) {
		return Organization{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		g.mu.Unlock()
		return Organization{}, err
	}
	raw, err := os.ReadFile(file)
	g.mu.Unlock()
	if err != nil {
		return Organization{}, err
	}
	spec, err := parseTenantOrganization(string(raw))
	if err != nil {
		return Organization{}, err
	}
	if spec.Locales, err = g.TenantLocales(ctx, tenant); err != nil {
		return Organization{}, err
	}
	out := Organization{
		DisplayName: spec.DisplayName,
		Country:     spec.Country,
		Locales:     spec.Locales,
		Missing:     spec.MissingOrganization(),
	}
	if out.Locales == nil {
		out.Locales = []string{}
	}
	if out.Missing == nil {
		out.Missing = []string{}
	}
	if spec.Organization != nil {
		out.Organization = *spec.Organization
	}
	return out, nil
}

// NormaliseOrganization trims every field, upper-cases the currency and
// checks each against what the CRD accepts, naming the field it refuses.
func NormaliseOrganization(org gentianov1alpha1.TenantOrganization) (gentianov1alpha1.TenantOrganization, error) {
	trim := func(s *string) { *s = strings.TrimSpace(*s) }
	for _, s := range []*string{&org.LegalName, &org.Currency, &org.TimeZone, &org.Website, &org.Email, &org.Phone, &org.TaxID, &org.RegistrationNumber} {
		trim(s)
	}
	if org.Address != nil {
		for _, s := range []*string{&org.Address.Street, &org.Address.PostalCode, &org.Address.City, &org.Address.Region} {
			trim(s)
		}
		if *org.Address == (gentianov1alpha1.TenantAddress{}) {
			org.Address = nil
		}
	}
	org.Currency = strings.ToUpper(org.Currency)
	refuse := func(field, why string) (gentianov1alpha1.TenantOrganization, error) {
		return gentianov1alpha1.TenantOrganization{}, fmt.Errorf("%w: %s %s", ErrInvalidOrganization, field, why)
	}
	if org.Currency != "" {
		if _, err := currency.ParseISO(org.Currency); err != nil || !currencyPattern.MatchString(org.Currency) {
			return refuse("currency", "is an ISO 4217 code, such as CHF or EUR")
		}
	}
	if org.TimeZone != "" {
		if _, err := time.LoadLocation(org.TimeZone); err != nil || org.TimeZone == "Local" {
			return refuse("timeZone", "is an IANA time zone, such as Europe/Zurich")
		}
	}
	if org.FiscalYearStartMonth < 0 || org.FiscalYearStartMonth > 12 {
		return refuse("fiscalYearStartMonth", "is a month, 1 to 12")
	}
	if org.Website != "" && !websitePattern.MatchString(org.Website) {
		return refuse("website", "is an http or https address")
	}
	if org.Email != "" {
		if a, err := mail.ParseAddress(org.Email); err != nil || a.Address != org.Email {
			return refuse("email", "is a plain address, such as info@example.com")
		}
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"legalName", org.LegalName, 200}, {"website", org.Website, 200}, {"email", org.Email, 200},
		{"phone", org.Phone, 40}, {"taxId", org.TaxID, 40}, {"registrationNumber", org.RegistrationNumber, 40},
	} {
		if len(f.value) > f.max {
			return refuse(f.name, fmt.Sprintf("is at most %d characters", f.max))
		}
	}
	if a := org.Address; a != nil {
		for _, f := range []struct {
			name, value string
			max         int
		}{
			{"address.street", a.Street, 200}, {"address.postalCode", a.PostalCode, 20},
			{"address.city", a.City, 100}, {"address.region", a.Region, 100},
		} {
			if len(f.value) > f.max {
				return refuse(f.name, fmt.Sprintf("is at most %d characters", f.max))
			}
		}
	}
	return org, nil
}

// CountryCurrency is the currency a country uses, or "" when it is not
// known: what an organisation that names no currency is given.
func CountryCurrency(country string) string {
	region, err := language.ParseRegion(country)
	if err != nil {
		return ""
	}
	unit, ok := currency.FromRegion(region)
	if !ok {
		return ""
	}
	return unit.String()
}

// SetTenantOrganization commits a tenant's display name and organisation.
// An empty display name leaves it as it is; it is required. A currency left
// empty is the one of the tenant's country, where it has one.
func (g *GitOps) SetTenantOrganization(ctx context.Context, tenant, displayName string, org gentianov1alpha1.TenantOrganization, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	displayName = strings.TrimSpace(displayName)
	if len(displayName) > 256 {
		return Result{}, fmt.Errorf("%w: displayName is at most 256 characters", ErrInvalidOrganization)
	}
	org, err := NormaliseOrganization(org)
	if err != nil {
		return Result{}, err
	}
	return g.apply(ctx, tenant, fmt.Sprintf("Set the organization of tenant %s", tenant), meta,
		func(text string) (string, string, bool, error) {
			have, err := parseTenantOrganization(text)
			if err != nil {
				return text, "", false, err
			}
			want := org
			if want.Currency == "" {
				want.Currency = CountryCurrency(have.Country)
			}
			name := displayName
			if name == "" {
				name = have.DisplayName
			}
			if name == have.DisplayName && sameOrganization(have.Organization, want) {
				return text, "unchanged", false, nil
			}
			out, err := setTenantOrganization(text, name, want)
			if err != nil {
				return text, "", false, err
			}
			return out, "updated", true, nil
		})
}

// setTenantOrganization writes spec.displayName and spec.organization into
// a tenant manifest's text and checks that they read back as written.
func setTenantOrganization(text, displayName string, org gentianov1alpha1.TenantOrganization) (string, error) {
	// A YAML double-quoted scalar takes Go's escapes; the read-back below
	// checks it.
	out, err := setSpecBlock(text, []string{"displayName"}, []string{"  displayName: " + strconv.Quote(displayName)})
	if err != nil {
		return "", err
	}
	var block []string
	if !sameOrganization(nil, org) {
		raw, err := yaml.Marshal(org)
		if err != nil {
			return "", err
		}
		block = []string{"  organization:"}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			block = append(block, "    "+line)
		}
	}
	if out, err = setSpecBlock(out, []string{"organization"}, block); err != nil {
		return "", err
	}
	back, err := parseTenantOrganization(out)
	if err != nil {
		return "", fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if back.DisplayName != displayName || !sameOrganization(back.Organization, org) {
		return "", errors.New("the edited manifest does not read back as written")
	}
	return out, nil
}

// sameOrganization says a manifest's organisation, nil when it has none, is
// the one wanted; an empty one is none.
func sameOrganization(have *gentianov1alpha1.TenantOrganization, want gentianov1alpha1.TenantOrganization) bool {
	if have == nil {
		have = &gentianov1alpha1.TenantOrganization{}
	}
	return reflect.DeepEqual(*have, want)
}
