package v1alpha1

import "strings"

// EffectiveEdition returns the edition for a profile, defaulting to full.
func EffectiveEdition(e Edition) Edition {
	if e == "" {
		return EditionFull
	}
	return e
}

// EffectiveTrustTier returns the trust tier for a profile, defaulting to certified.
func EffectiveTrustTier(t TrustTier) TrustTier {
	if t == "" {
		return TrustTierCertified
	}
	return t
}

// EffectiveCatalogueVersion returns the catalogue semver, defaulting to 1.0.0.
func EffectiveCatalogueVersion(v string) string {
	if v == "" {
		return DefaultCatalogueVersion
	}
	return v
}

// ProfileFamily returns the logical family for an AppProfile.
func ProfileFamily(p *AppProfile) string {
	if p.Spec.Family != "" {
		return p.Spec.Family
	}
	return p.Name
}

// ProfileIdentityFor returns the normalized catalogue identity for an AppProfile.
func ProfileIdentityFor(p *AppProfile) ProfileIdentity {
	return ProfileIdentity{
		Family:           ProfileFamily(p),
		CatalogueVersion: EffectiveCatalogueVersion(p.Spec.CatalogueVersion),
		Edition:          EffectiveEdition(p.Spec.Edition),
	}
}

// ProfileCatalogueLabels returns index labels for an AppProfile's catalogue metadata.
func ProfileCatalogueLabels(p *AppProfile) map[string]string {
	id := ProfileIdentityFor(p)
	return map[string]string{
		LabelProfileName:             p.Name,
		LabelProfileFamily:           id.Family,
		LabelProfileCatalogueVersion: id.CatalogueVersion,
		LabelProfileEdition:          string(id.Edition),
		LabelProfileTrustTier:        string(EffectiveTrustTier(p.Spec.TrustTier)),
	}
}

// ProfileReferenceKey returns a stable string key for deduplication (name or identity tuple).
func ProfileReferenceKey(ref ProfileReference) string {
	if ref.Name != "" {
		return "name:" + ref.Name
	}
	if ref.Identity == nil {
		return ""
	}
	id := *ref.Identity
	return "id:" + id.Family + "/" + EffectiveCatalogueVersion(id.CatalogueVersion) +
		"/" + string(EffectiveEdition(id.Edition))
}

// ResolveProfileReference finds the AppProfile name matching ref in profiles.
func ResolveProfileReference(profiles []AppProfile, ref ProfileReference) (string, bool) {
	if ref.Name != "" {
		for i := range profiles {
			if profiles[i].Name == ref.Name {
				return profiles[i].Name, true
			}
		}
		return "", false
	}
	if ref.Identity == nil {
		return "", false
	}
	var match string
	for i := range profiles {
		if !ProfileReferenceMatches(ref, &profiles[i]) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = profiles[i].Name
	}
	return match, match != ""
}

// ProfileReferenceMatches reports whether ref resolves to profile p.
func ProfileReferenceMatches(ref ProfileReference, p *AppProfile) bool {
	if ref.Name != "" {
		return ref.Name == p.Name
	}
	if ref.Identity == nil {
		return false
	}
	id := ProfileIdentityFor(p)
	want := *ref.Identity
	return id.Family == want.Family &&
		id.CatalogueVersion == EffectiveCatalogueVersion(want.CatalogueVersion) &&
		id.Edition == EffectiveEdition(want.Edition)
}

// ProfileRequiresEntitlement reports whether CRM entitlement is required before install.
// Premium profiles in gentian-premium use license: proprietary.
func ProfileRequiresEntitlement(p *AppProfile) bool {
	return strings.EqualFold(p.Spec.License, "proprietary")
}

// EffectiveDeploymentRole reads gentianos.io/deployment-role (default: standalone).
func EffectiveDeploymentRole(p *AppProfile) ProfileDeploymentRole {
	if p == nil {
		return ProfileDeploymentRoleStandalone
	}
	switch strings.ToLower(strings.TrimSpace(p.Annotations[AnnotationProfileDeploymentRole])) {
	case string(ProfileDeploymentRoleBase):
		return ProfileDeploymentRoleBase
	case string(ProfileDeploymentRoleModule):
		return ProfileDeploymentRoleModule
	default:
		return ProfileDeploymentRoleStandalone
	}
}

// ProfileRequiresProfile returns the base profile name from gentianos.io/requires-profile.
// Used with deployment-role=module so the operator can auto-install shared runtimes.
func ProfileRequiresProfile(p *AppProfile) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Annotations[AnnotationProfileRequiresProfile])
}
