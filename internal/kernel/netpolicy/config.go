/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Config carries cluster-specific namespace names and options used when
// building tenant isolation NetworkPolicies.
type Config struct {
	ServicesNamespace string
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
		ServicesNamespace: layout.Kernel(layout.Edge),
		KubeAPIServerCIDR: "10.0.0.0/8",
	}
}
