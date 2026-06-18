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
	if id.OfferingTier != v1alpha1.OfferingTierFree {
		t.Errorf("offeringTier: got %q", id.OfferingTier)
	}
}

func TestResolveProfileReference_ByName(t *testing.T) {
	profiles := []v1alpha1.AppProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "openproject"}},
	}
	name, ok := v1alpha1.ResolveProfileReference(profiles, v1alpha1.ProfileReference{Name: "openproject"})
	if !ok || name != "openproject" {
		t.Fatalf("resolve by name: got %q, %v", name, ok)
	}
}

func TestResolveProfileReference_ByIdentity(t *testing.T) {
	profiles := []v1alpha1.AppProfile{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "openproject-2.1.0-full-supported"},
			Spec: v1alpha1.AppProfileSpec{
				Family:           "openproject",
				CatalogueVersion: "2.1.0",
				Edition:          v1alpha1.EditionFull,
				OfferingTier:     v1alpha1.OfferingTierSupported,
			},
		},
	}
	ref := v1alpha1.ProfileReference{
		Identity: &v1alpha1.ProfileIdentity{
			Family:           "openproject",
			CatalogueVersion: "2.1.0",
			Edition:          v1alpha1.EditionFull,
			OfferingTier:     v1alpha1.OfferingTierSupported,
		},
	}
	name, ok := v1alpha1.ResolveProfileReference(profiles, ref)
	if !ok || name != "openproject-2.1.0-full-supported" {
		t.Fatalf("resolve by identity: got %q, %v", name, ok)
	}
}

func TestProfileReferenceKey(t *testing.T) {
	key := v1alpha1.ProfileReferenceKey(v1alpha1.ProfileReference{Name: "element"})
	if key != "name:element" {
		t.Errorf("got %q", key)
	}
}
