/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// EffectiveContractCapabilities returns what a consumer was granted of a
// contract, or nil when it was granted nothing.
//
// A profile that names a contract under integrations asks for a relation to
// another app; it does not have one. The relation exists when the tenant's
// administrator grants it: an AppGrant for the consumer that names the
// contract and at least one capability. Without that, nothing is opened,
// whatever the two profiles declare.
func EffectiveContractCapabilities(
	binding *gentianov1alpha1.IntegrationBinding,
	grant *gentianov1alpha1.AppGrant,
) []string {
	if binding == nil || grant == nil {
		return nil
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
	return nil
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
