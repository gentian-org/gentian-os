package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestProfileIdentityFor_Defaults(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "openproject"},
		Spec:       v1alpha1.AppProfileSpec{DisplayName: "OpenProject"},
	}
	id := v1alpha1.ProfileIdentityFor(p)
	if id.Family != "openproject" {
		t.Errorf("family: got %q", id.Family)
	}
	if id.CatalogueVersion != "1.0.0" {
		t.Errorf("catalogueVersion: got %q", id.CatalogueVersion)
	}
	if id.Edition != v1alpha1.EditionFull {
		t.Errorf("edition: got %q", id.Edition)
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

func TestProfileCatalogueLabels(t *testing.T) {
	p := &v1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "openproject"},
		Spec: v1alpha1.AppProfileSpec{
			Family:           "openproject",
			CatalogueVersion: "1.0.0",
			Edition:          v1alpha1.EditionFull,
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
			ObjectMeta: metav1.ObjectMeta{Name: "openproject-2.1.0-full"},
			Spec: v1alpha1.AppProfileSpec{
				Family:           "openproject",
				CatalogueVersion: "2.1.0",
				Edition:          v1alpha1.EditionFull,
			},
		},
	}
	ref := v1alpha1.ProfileReference{
		Identity: &v1alpha1.ProfileIdentity{
			Family:           "openproject",
			CatalogueVersion: "2.1.0",
			Edition:          v1alpha1.EditionFull,
		},
	}
	name, ok := v1alpha1.ResolveProfileReference(profiles, ref)
	if !ok || name != "openproject-2.1.0-full" {
		t.Fatalf("resolve by identity: got %q, %v", name, ok)
	}
}
