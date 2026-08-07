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

package v1alpha1

import (
	"encoding/json"
	"strings"
)

// EffectiveEdition returns the edition for a profile, defaulting to ce.
//
// ce is the right default: an entry that does not say otherwise is the upstream
// community edition. Defaulting to me or pro would silently claim maintenance or
// a commercial relationship that nobody declared.
func EffectiveEdition(e Edition) Edition {
	if e == "" {
		return EditionCE
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
// Premium profiles in gentian-pro use license: proprietary.
func ProfileRequiresEntitlement(p *AppProfile) bool {
	return strings.EqualFold(p.Spec.License, "proprietary")
}

// ProfileIsAPI reports whether the profile is an ApiProfile (deploymentMethod: api):
// a catalogue entry backed by an external service that runs no workload pods.
func ProfileIsAPI(p *AppProfile) bool {
	return p != nil && p.Spec.DeploymentMethod == DeploymentMethodAPI
}

// ProfileDeploysWorkload reports whether the orchestrator should create a tenant
// workload (Crossplane App claim / Helm release) for this profile. ApiProfiles
// contribute only catalogue and portal metadata and deploy no workload.
func ProfileDeploysWorkload(p *AppProfile) bool {
	return !ProfileIsAPI(p)
}

// EffectiveDeploymentRole reads gentianos.io/deployment-role (default: standalone).
func EffectiveDeploymentRole(p *AppProfile) ProfileDeploymentRole {
	if p == nil {
		return ProfileDeploymentRoleStandalone
	}
	switch strings.ToLower(strings.TrimSpace(p.Annotations[AnnotationProfileDeploymentRole])) {
	case string(ProfileDeploymentRoleBase):
		return ProfileDeploymentRoleBase
	// "module" is the deprecated spelling of "addon" and normalises to it, so callers
	// only ever compare against Addon and the catalogue can migrate incrementally.
	case string(ProfileDeploymentRoleAddon), string(ProfileDeploymentRoleModule):
		return ProfileDeploymentRoleAddon
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

// GatewayAPIBackend is one extra HTTPRoute rule: path prefix → Kubernetes Service.
type GatewayAPIBackend struct {
	PathPrefix  string `json:"pathPrefix"`
	ServiceName string `json:"serviceName"`
	Port        int32  `json:"port,omitempty"`
}

// ProfileGatewayRootRedirect returns gentianos.io/gateway-root-redirect when set.
func ProfileGatewayRootRedirect(p *AppProfile) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Annotations[AnnotationProfileGatewayRootRedirect])
}

// ProfileGatewayAPIBackends parses gentianos.io/gateway-api-backends JSON.
func ProfileGatewayAPIBackends(p *AppProfile) ([]GatewayAPIBackend, error) {
	if p == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileGatewayAPIBackends])
	if raw == "" {
		return nil, nil
	}
	var backends []GatewayAPIBackend
	if err := json.Unmarshal([]byte(raw), &backends); err != nil {
		return nil, err
	}
	return backends, nil
}

// ProfileOIDCDefaultRedirectURIs parses gentianos.io/oidc-default-redirect-uris JSON.
func ProfileOIDCDefaultRedirectURIs(p *AppProfile) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileOIDCDefaultRedirectURIs])
	if raw == "" {
		return nil, nil
	}
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		return nil, err
	}
	return uris, nil
}

// GatewayFrameAncestorsSpec is the JSON shape for gentianos.io/gateway-frame-ancestors.
type GatewayFrameAncestorsSpec struct {
	Mode    string   `json:"mode"`
	Origins []string `json:"origins"`
}

// IngressGatewayFrameAncestors parses gentianos.io/gateway-frame-ancestors on an ingress.
func IngressGatewayFrameAncestors(ingress *IngressSpec) (*GatewayFrameAncestorsSpec, error) {
	if ingress == nil || len(ingress.Annotations) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(ingress.Annotations[AnnotationIngressGatewayFrameAncestors])
	if raw == "" {
		return nil, nil
	}
	var spec GatewayFrameAncestorsSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// IngressGatewayEscapedSlashesAction returns gentianos.io/gateway-escaped-slashes-action when set.
func IngressGatewayEscapedSlashesAction(ingress *IngressSpec) string {
	if ingress == nil || len(ingress.Annotations) == 0 {
		return ""
	}
	return strings.TrimSpace(ingress.Annotations[AnnotationIngressGatewayEscapedSlashesAction])
}

// ProfileKernelEgressNamespaces parses gentianos.io/kernel-egress-namespaces on an AppProfile.
func ProfileKernelEgressNamespaces(p *AppProfile) []string {
	if p == nil || len(p.Annotations) == 0 {
		return nil
	}
	raw := strings.TrimSpace(p.Annotations[AnnotationProfileKernelEgressNamespaces])
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
