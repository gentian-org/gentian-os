/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import "strings"

// The tokens a profile writes in its values, scripts and addresses, which the
// render replaces with facts about the app's own context: its tenant,
// namespace and domain, and the cluster's kernel domain and node address.
// Never an endpoint: a token names nothing to connect to and carries nothing
// to authenticate with.

// placeholders are the app's own tokens: ${TENANT_DOMAIN},
// ${TENANT_NAMESPACE}, ${TENANT_ID} and ${APP_ID}, as token and value pairs.
func (in *Input) placeholders() []string {
	return []string{
		"${TENANT_DOMAIN}", in.TenantDomain(),
		"${TENANT_NAMESPACE}", in.Namespace,
		"${TENANT_ID}", in.Tenant(),
		"${APP_ID}", in.App,
	}
}

// clusterPlaceholders are ${KERNEL_DOMAIN} and ${NODE_IP}, each only when
// the cluster states it: a token with no value stays as it is written.
func (in *Input) clusterPlaceholders() []string {
	var pairs []string
	if d := in.KernelServices["KERNEL_DOMAIN"]; d != "" {
		pairs = append(pairs, "${KERNEL_DOMAIN}", d)
	}
	if ip := in.ClusterConfig["node.ip"]; ip != "" {
		pairs = append(pairs, "${NODE_IP}", ip)
	}
	return pairs
}

// expand replaces each token of pairs (token, value, token, value, ...) in s,
// one token after the other.
func expand(s string, pairs ...string) string {
	for i := 0; i+1 < len(pairs); i += 2 {
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	return s
}
