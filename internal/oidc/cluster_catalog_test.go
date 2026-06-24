// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestResolvePackClusterCatalog(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	catalog := &gentianov1alpha1.OIDCPackCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "opendesk"},
		Spec: gentianov1alpha1.OIDCPackCatalogSpec{
			MapperTemplates: map[string]gentianov1alpha1.OIDCMapperTemplate{
				"opendesk_useruuid": {
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"claim.name": "opendesk_useruuid",
					},
				},
			},
			Packs: map[string]gentianov1alpha1.OIDCPackSpec{
				"opendesk-jitsi": {
					ScopeName:     "opendesk-jitsi-scope",
					ClientRole:    "opendesk-jitsi-access-control",
					LDAPGroup:     "managed-by-attribute-Videoconference",
					PublicClient:  true,
					DefaultScopes: []string{"profile", "email"},
					Mappers:       []string{"opendesk_useruuid"},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(catalog).Build()

	pack, templates, ok, err := ResolvePack(context.Background(), c, "opendesk-jitsi")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected pack from cluster catalog")
	}
	if pack.ScopeName != "opendesk-jitsi-scope" {
		t.Fatalf("scopeName: %q", pack.ScopeName)
	}
	if _, found := templates["opendesk_useruuid"]; !found {
		t.Fatal("missing mapper template")
	}
}

func TestResolvePackEmbedFallback(t *testing.T) {
	pack, _, ok, err := ResolvePack(context.Background(), nil, "opendesk-openproject")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected embedded pack fallback")
	}
	if pack.LDAPGroup != "managed-by-attribute-Projectmanagement" {
		t.Fatalf("ldapGroup: %q", pack.LDAPGroup)
	}
}
