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

package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestProfileIdentityFor_Defaults(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-app"},
		Spec:       v1alpha1.AppProfileSpec{DisplayName: "Demo App"},
	}
	id := v1alpha1.ProfileIdentityFor(p)
	if id.Family != "demo-app" {
		t.Errorf("family: got %q", id.Family)
	}
	if id.CatalogueVersion != "1.0.0" {
		t.Errorf("catalogueVersion: got %q", id.CatalogueVersion)
	}
	if id.Edition != v1alpha1.EditionCE {
		t.Errorf("edition: got %q", id.Edition)
	}
}

func TestEffectiveDeploymentRole(t *testing.T) {
	base := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{v1alpha1.AnnotationProfileDeploymentRole: "base"},
		},
	}
	if v1alpha1.EffectiveDeploymentRole(base) != v1alpha1.ProfileDeploymentRoleBase {
		t.Fatalf("role: got %q", v1alpha1.EffectiveDeploymentRole(base))
	}
	if v1alpha1.EffectiveDeploymentRole(&v1alpha1.AppProfile{}) != v1alpha1.ProfileDeploymentRoleStandalone {
		t.Fatal("expected standalone default")
	}
}

func TestProfileGatewayAnnotations(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1alpha1.AnnotationProfileGatewayRootRedirect: "/app/",
				v1alpha1.AnnotationProfileGatewayAPIBackends:  `[{"pathPrefix":"/app/api","serviceName":"demo-api"}]`,
			},
		},
	}
	if v1alpha1.ProfileGatewayRootRedirect(p) != "/app/" {
		t.Fatalf("root redirect: %q", v1alpha1.ProfileGatewayRootRedirect(p))
	}
	backends, err := v1alpha1.ProfileGatewayAPIBackends(p)
	if err != nil || len(backends) != 1 || backends[0].ServiceName != "demo-api" {
		t.Fatalf("api backends: %v, %v", backends, err)
	}
}

func TestProfileOIDCDefaultRedirectURIs(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				v1alpha1.AnnotationProfileOIDCDefaultRedirectURIs: `["https://demo.${TENANT_DOMAIN}/oidc/callback"]`,
			},
		},
	}
	uris, err := v1alpha1.ProfileOIDCDefaultRedirectURIs(p)
	if err != nil || len(uris) != 1 || uris[0] != "https://demo.${TENANT_DOMAIN}/oidc/callback" {
		t.Fatalf("oidc defaults: %v, %v", uris, err)
	}
}

func TestProfileRequiresEntitlement(t *testing.T) {
	oss := &v1alpha1.AppProfile{
		Spec: v1alpha1.AppProfileSpec{License: "Apache-2.0"},
	}
	if v1alpha1.ProfileRequiresEntitlement(oss) {
		t.Error("OSS license should not require entitlement")
	}
	premium := &v1alpha1.AppProfile{
		Spec: v1alpha1.AppProfileSpec{License: "proprietary"},
	}
	if !v1alpha1.ProfileRequiresEntitlement(premium) {
		t.Error("proprietary license should require entitlement")
	}
}

func TestProfileIsAPIAndDeploysWorkload(t *testing.T) {
	api := &v1alpha1.AppProfile{
		Spec: v1alpha1.AppProfileSpec{DeploymentMethod: v1alpha1.DeploymentMethodAPI},
	}
	if !v1alpha1.ProfileIsAPI(api) {
		t.Error("deploymentMethod api should be an ApiProfile")
	}
	if v1alpha1.ProfileDeploysWorkload(api) {
		t.Error("ApiProfile should not deploy a workload")
	}

	for _, m := range []v1alpha1.DeploymentMethod{v1alpha1.DeploymentMethodCrossplane, v1alpha1.DeploymentMethodArgoCD, ""} {
		p := &v1alpha1.AppProfile{Spec: v1alpha1.AppProfileSpec{DeploymentMethod: m}}
		if v1alpha1.ProfileIsAPI(p) {
			t.Errorf("deploymentMethod %q should not be an ApiProfile", m)
		}
		if !v1alpha1.ProfileDeploysWorkload(p) {
			t.Errorf("deploymentMethod %q should deploy a workload", m)
		}
	}

	if v1alpha1.ProfileIsAPI(nil) {
		t.Error("nil profile should not be an ApiProfile")
	}
}

func TestProfileCatalogueLabels(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-app"},
		Spec: v1alpha1.AppProfileSpec{
			Family:           "demo-app",
			CatalogueVersion: "1.0.0",
			Edition:          v1alpha1.EditionCE,
			TrustTier:        v1alpha1.TrustTierCertified,
			License:          "Apache-2.0",
		},
	}
	labels := v1alpha1.ProfileCatalogueLabels(p)
	if labels[v1alpha1.LabelProfileTrustTier] != "certified" {
		t.Errorf("trust label: got %q", labels[v1alpha1.LabelProfileTrustTier])
	}
}

func TestResolveProfileReference_ByIdentity(t *testing.T) {
	profiles := []v1alpha1.AppProfile{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-app-2.1.0-full"},
			Spec: v1alpha1.AppProfileSpec{
				Family:           "demo-app",
				CatalogueVersion: "2.1.0",
				Edition:          v1alpha1.EditionCE,
			},
		},
	}
	ref := v1alpha1.ProfileReference{
		Identity: &v1alpha1.ProfileIdentity{
			Family:           "demo-app",
			CatalogueVersion: "2.1.0",
			Edition:          v1alpha1.EditionCE,
		},
	}
	name, ok := v1alpha1.ResolveProfileReference(profiles, ref)
	if !ok || name != "demo-app-2.1.0-full" {
		t.Fatalf("resolve by identity: got %q, %v", name, ok)
	}
}

// TestEffectiveDeploymentRoleAddonAlias locks in the migration alias: "module" is
// the pre-cleanup spelling of "addon" and must normalise to Addon, so callers only
// ever compare against one value and the catalogue can migrate profile-by-profile
// instead of in a flag day.
func TestEffectiveDeploymentRoleAddonAlias(t *testing.T) {
	for _, annotation := range []string{"addon", "module", "Addon", " module "} {
		p := &v1alpha1.AppProfile{}
		p.Annotations = map[string]string{v1alpha1.AnnotationProfileDeploymentRole: annotation}
		if got := v1alpha1.EffectiveDeploymentRole(p); got != v1alpha1.ProfileDeploymentRoleAddon {
			t.Fatalf("annotation %q: got %q, want %q", annotation, got, v1alpha1.ProfileDeploymentRoleAddon)
		}
	}
	// base and unset must be unaffected by the alias
	base := &v1alpha1.AppProfile{}
	base.Annotations = map[string]string{v1alpha1.AnnotationProfileDeploymentRole: "base"}
	if v1alpha1.EffectiveDeploymentRole(base) != v1alpha1.ProfileDeploymentRoleBase {
		t.Fatal("base role regressed")
	}
	if v1alpha1.EffectiveDeploymentRole(&v1alpha1.AppProfile{}) != v1alpha1.ProfileDeploymentRoleStandalone {
		t.Fatal("unset role regressed")
	}
}
