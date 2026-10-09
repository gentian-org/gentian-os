/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import "github.com/gentian-org/gentian-os/internal/meta"

const RoutingModeGateway = meta.RoutingModeGateway

// Config carries cluster-specific namespace names and routing options used when
// building tenant isolation NetworkPolicies.
type Config struct {
	InfraNamespace    string
	ServicesNamespace string
	OpenbaoNamespace  string
	RoutingMode       string
	KubeAPIServerCIDR string
	// NarrowEdge admits, from the edge namespace, the Gateway's Envoy pods
	// alone instead of every pod there. It follows the switch the kernel
	// namespaces' own rules are under (KERNEL_NETWORK_POLICIES): one setting
	// decides both, so a cluster that turned the rules off to rule them out
	// has ruled this one out too.
	NarrowEdge bool
}

// DefaultConfig returns test-friendly defaults; production callers should pass explicit Config.
func DefaultConfig() Config {
	return Config{
		InfraNamespace:    "gentian-infra-dev",
		ServicesNamespace: "gentian-dev",
		OpenbaoNamespace:  "openbao",
		RoutingMode:       RoutingModeGateway,
		KubeAPIServerCIDR: "10.0.0.0/8",
	}
}
