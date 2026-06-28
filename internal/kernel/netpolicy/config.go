/*
Copyright 2026 The Gentian Authors.

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
}

// DefaultConfig returns test-friendly defaults; production callers should pass explicit Config.
func DefaultConfig() Config {
	return Config{
		InfraNamespace:    "gentian-infra-dev",
		ServicesNamespace: "gentian-dev",
		OpenbaoNamespace:  "openbao",
		RoutingMode:       RoutingModeGateway,
		KubeAPIServerCIDR: "10.96.0.0/12",
	}
}
