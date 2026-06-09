// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package stagingca

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testLeafPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestBuildBundleIncludesLeaf(t *testing.T) {
	leaf := testLeafPEM(t)
	bundle, err := BuildBundle(leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if len(bundle) < len(leaf) {
		t.Fatalf("expected bundle to include leaf cert")
	}
}

func TestEnsureStagingCASecretNoOpWithoutLeaf(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ok, err := EnsureStagingCASecret(context.Background(), c, "gentian-dev", "cert-manager", "wildcard-kernel-tls")
	if err != nil {
		t.Fatalf("EnsureStagingCASecret: %v", err)
	}
	if ok {
		t.Fatal("expected false when leaf secret is absent")
	}
}

func TestEnsureStagingCASecretCreatesTargetSecret(t *testing.T) {
	leafPEM := testLeafPEM(t)
	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultLeafSecret, Namespace: DefaultCertManagerNS},
		Data:       map[string][]byte{"tls.crt": leafPEM},
	}).Build()

	ok, err := EnsureStagingCASecret(context.Background(), c, "gentian-dev", DefaultCertManagerNS, DefaultLeafSecret)
	if err != nil {
		t.Fatalf("EnsureStagingCASecret: %v", err)
	}
	if !ok {
		t.Fatal("expected true when secret was created")
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: SecretName, Namespace: "gentian-dev"}, got); err != nil {
		t.Fatalf("get created secret: %v", err)
	}
	if len(got.Data["ca.crt"]) == 0 {
		t.Fatal("expected non-empty ca.crt")
	}
	if len(got.Data[TrustStoreKey]) == 0 {
		t.Fatal("expected non-empty truststore.jks")
	}
}
