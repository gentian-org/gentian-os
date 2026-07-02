// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
