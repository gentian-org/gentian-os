package v1alpha1

// EffectiveEdition returns the edition for a profile, defaulting to full.
func EffectiveEdition(e Edition) Edition {
	if e == "" {
		return EditionFull
	}
	return e
}

// EffectiveOfferingTier returns the offering tier for a profile, defaulting to free.
func EffectiveOfferingTier(t OfferingTier) OfferingTier {
	if t == "" {
		return OfferingTierFree
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
		OfferingTier:     EffectiveOfferingTier(p.Spec.OfferingTier),
	}
}

// ProfileCatalogueLabels returns index labels for an AppProfile's catalogue identity.
func ProfileCatalogueLabels(p *AppProfile) map[string]string {
	id := ProfileIdentityFor(p)
	return map[string]string{
		LabelProfileName:             p.Name,
		LabelProfileFamily:         id.Family,
		LabelProfileCatalogueVersion: id.CatalogueVersion,
		LabelProfileEdition:        string(id.Edition),
		LabelProfileOfferingTier:   string(id.OfferingTier),
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
		"/" + string(EffectiveEdition(id.Edition)) + "/" + string(EffectiveOfferingTier(id.OfferingTier))
}

// EffectiveTrustTier returns the trust tier for a product, defaulting to certified.
func EffectiveTrustTier(t TrustTier) TrustTier {
	if t == "" {
		return TrustTierCertified
	}
	return t
}

// EffectiveListable returns whether a product is listable in the store.
func EffectiveListable(listable *bool) bool {
	if listable == nil {
		return true
	}
	return *listable
}

// ResolveProfileReference finds the AppProfile name matching ref in profiles.
// Returns the profile name and true when exactly one match exists.
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
			return "", false // ambiguous
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
		id.Edition == EffectiveEdition(want.Edition) &&
		id.OfferingTier == EffectiveOfferingTier(want.OfferingTier)
}
