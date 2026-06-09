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
	"net/http"
	"net/http/httptest"
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

func testIssuerChainServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate root key: %v", err)
	}
	rootTmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "staging-root"},
		IsCA:         true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, &rootTmpl, &rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})

	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	intKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate intermediate key: %v", err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	var intDER []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/intermediate":
			_, _ = w.Write(intDER)
		case "/root":
			_, _ = w.Write(rootDER)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	intTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "staging-intermediate"},
		IsCA:                  true,
		IssuingCertificateURL: []string{srv.URL + "/root"},
	}
	intDER, err = x509.CreateCertificate(rand.Reader, &intTmpl, rootCert, &intKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create intermediate: %v", err)
	}
	intCert, err := x509.ParseCertificate(intDER)
	if err != nil {
		t.Fatalf("parse intermediate: %v", err)
	}

	leafTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "server.local"},
		IssuingCertificateURL: []string{srv.URL + "/intermediate"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &leafTmpl, intCert, &leafKey.PublicKey, intKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	_ = rootPEM
	return srv, leafPEM
}

func TestBuildBundleExcludesServerLeaf(t *testing.T) {
	_, leaf := testIssuerChainServer(t)

	bundle, err := BuildBundle(context.Background(), leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if containsPEMCN(bundle.NodeExtraCA, "server.local") {
		t.Fatal("staging chain must not include the server leaf")
	}
	if !containsPEMCN(bundle.NodeExtraCA, "staging-intermediate") {
		t.Fatal("expected staging intermediate in node-extra-ca chain")
	}
	if !containsPEMCN(bundle.NodeExtraCA, "staging-root") {
		t.Fatal("expected staging root in node-extra-ca chain")
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
	_, leafPEM := testIssuerChainServer(t)
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
	if len(got.Data[NodeExtraCAKey]) == 0 {
		t.Fatal("expected node-extra-ca.crt")
	}
	if len(got.Data[TrustStoreKey]) == 0 {
		t.Fatal("expected non-empty truststore.jks")
	}
}

func containsPEMCN(pemData []byte, cn string) bool {
	rest := pemData
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return false
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if cert.Subject.CommonName == cn {
			return true
		}
	}
}
