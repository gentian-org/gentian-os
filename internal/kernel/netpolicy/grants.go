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

package netpolicy

import (
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// EffectiveContractCapabilities returns the MAC-allowed capability list for a
// consumer→provider binding after intersecting with AppGrant (when present).
func EffectiveContractCapabilities(
	binding *gentianov1alpha1.IntegrationBinding,
	grant *gentianov1alpha1.AppGrant,
) []string {
	if binding == nil {
		return nil
	}
	declared := append([]string(nil), binding.Spec.Capabilities...)
	if grant == nil {
		return declared
	}
	for _, consume := range grant.Spec.Consume {
		if consume.Contract != binding.Spec.Contract {
			continue
		}
		if len(consume.Granted) == 0 {
			return nil
		}
		return append([]string(nil), consume.Granted...)
	}
	return declared
}

// FormatCapabilityLabel joins capabilities for NetworkPolicy labels (max 63 chars).
// Capability names use colons (e.g. webdav:read); Kubernetes label values allow
// only alphanumerics plus '-', '_', and '.' — no colons or commas.
func FormatCapabilityLabel(caps []string) string {
	if len(caps) == 0 {
		return ""
	}
	sanitized := make([]string, len(caps))
	for i, c := range caps {
		sanitized[i] = strings.NewReplacer(":", "_", "/", "_", ",", "_").Replace(c)
	}
	label := strings.Join(sanitized, ".")
	if len(label) > 63 {
		label = label[:63]
	}
	return label
}
