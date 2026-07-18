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

package controller

import (
	"fmt"
	"strings"
)

// keycloakOIDCAncestorOrigins builds space-separated https origins for the
// Keycloak HTTPRoute frame-ancestors policy: kernel portal plus, per tenant
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
	add(fmt.Sprintf("https://portal.%s", kernelDomain))
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
