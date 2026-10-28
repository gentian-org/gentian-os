/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

// OrganizationRequired are the facts about a tenant an app cannot set itself
// up without, by their place in the Tenant's spec. Tenant Settings asks for
// them first and the App Store says so while any is missing.
var OrganizationRequired = []string{
	"organization.legalName",
	"country",
	"organization.currency",
	"organization.timeZone",
	"locales",
}

// MissingOrganization names the required facts this spec does not give, in
// the order of OrganizationRequired; empty when it gives them all.
func (s *TenantSpec) MissingOrganization() []string {
	org := s.Organization
	if org == nil {
		org = &TenantOrganization{}
	}
	given := map[string]bool{
		"organization.legalName": org.LegalName != "",
		"country":                s.Country != "",
		"organization.currency":  org.Currency != "",
		"organization.timeZone":  org.TimeZone != "",
		"locales":                len(s.Locales) > 0,
	}
	var missing []string
	for _, field := range OrganizationRequired {
		if !given[field] {
			missing = append(missing, field)
		}
	}
	return missing
}
