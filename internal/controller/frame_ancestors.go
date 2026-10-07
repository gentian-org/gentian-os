/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"strings"
)

// keycloakOIDCAncestorOrigins builds space-separated https origins for the
// Keycloak HTTPRoute frame-ancestors policy: the platform desktop plus, per tenant
// effective domain, a tenant wildcard and explicit OIDC app ingress hosts
// discovered from installed AppProfiles (see collectOIDCIngressSubdomainsByTenant).
func keycloakOIDCAncestorOrigins(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
) string {
	if kernelDomain == "" {
		return ""
	}
	seen := make(map[string]struct{})
	var origins []string
	add := func(origin string) {
		if origin == "" {
			return
		}
		if _, ok := seen[origin]; ok {
			return
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	add("https://" + platformDesktopHost(kernelDomain))
	add(fmt.Sprintf("https://id.%s", kernelDomain))
	add(fmt.Sprintf("https://*.%s", kernelDomain))
	for i, effective := range tenantEffectiveDomains {
		if effective == "" {
			continue
		}
		add(fmt.Sprintf("https://*.%s", effective))
		var tenantName string
		if i < len(tenantNames) {
			tenantName = tenantNames[i]
		}
		for _, sub := range tenantOIDCSubdomains[tenantName] {
			if sub == "@" {
				add(fmt.Sprintf("https://%s", effective))
			} else {
				add(fmt.Sprintf("https://%s.%s", sub, effective))
			}
		}
	}
	return strings.Join(origins, " ")
}
