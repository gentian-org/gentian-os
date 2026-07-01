// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package security

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// WaiverKey uniquely identifies a MAC waiver request or approval.
type WaiverKey struct {
	Profile string
	Policy  string
	Scope   string
}

func waiverKey(profile, policy, scope string) WaiverKey {
	return WaiverKey{Profile: profile, Policy: policy, Scope: scope}
}

// ApprovedMacWaivers returns profile requests intersected with the cluster allowlist.
func ApprovedMacWaivers(
	profile string,
	requests []gentianov1alpha1.MacWaiverRequest,
	allowed []gentianov1alpha1.AllowedMacWaiver,
) []gentianov1alpha1.MacWaiverRequest {
	if len(requests) == 0 || len(allowed) == 0 {
		return nil
	}
	allowSet := make(map[WaiverKey]struct{}, len(allowed))
	for _, a := range allowed {
		allowSet[waiverKey(a.Profile, a.Policy, a.Scope)] = struct{}{}
	}
	var out []gentianov1alpha1.MacWaiverRequest
	for _, req := range requests {
		if _, ok := allowSet[waiverKey(profile, req.Policy, req.Scope)]; ok {
			out = append(out, req)
		}
	}
	return out
}

// IsWaiverApproved reports whether a specific waiver is in the approved set.
func IsWaiverApproved(approved []gentianov1alpha1.MacWaiverRequest, policy, scope string) bool {
	for _, w := range approved {
		if w.Policy == policy && w.Scope == scope {
			return true
		}
	}
	return false
}
