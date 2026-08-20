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

// Package stagingca builds the gentian-staging-ca-tls trust bundle for ACME
// staging clusters. Catalogue apps mount this secret so in-cluster OIDC
// clients trust https://id.<kernel-domain>.
package stagingca

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SecretName is replicated into each tenant namespace by the operator.
	SecretName = "gentian-staging-ca-tls"
	// NodeExtraCAKey holds the LE staging CA chain for NODE_EXTRA_CA_CERTS.
	// Node.js appends this file to the default Mozilla trust store; it must
	// contain the full staging issuer chain (not the server leaf or a duplicate
	// Mozilla bundle). See docs/design/security.md §9.1.
	NodeExtraCAKey = "node-extra-ca.crt"

	DefaultCertManagerNS = "cert-manager"
	DefaultLeafSecret    = "wildcard-kernel-tls"

	mozillaCABundleURL = "https://curl.se/ca/cacert.pem"
	maxStagingCAChain  = 8
)

// Bundle holds the PEM material written to gentian-staging-ca-tls.
type Bundle struct {
	// CACrt is the Mozilla CA bundle plus the LE staging issuer chain (for
	// curl --cacert, Java truststore, REQUESTS_CA_BUNDLE).
	CACrt []byte
	// NodeExtraCA is the LE staging issuer chain only (for NODE_EXTRA_CA_CERTS).
	NodeExtraCA []byte
}

// BuildBundle returns trust bundles for ACME staging clusters. CACrt matches
// scripts/bootstrap/create-staging-ca-secret.sh (system CAs + LE staging chain via AIA).
// NodeExtraCA contains only the staging issuer chain for Node.js clients.
func BuildBundle(ctx context.Context, leafPEM []byte) (*Bundle, error) {
	if len(leafPEM) == 0 {
		return nil, fmt.Errorf("empty leaf certificate")
	}
	stagingChain, err := fetchStagingCAChain(leafPEM)
	if err != nil {
		return nil, err
	}
	mozilla, err := loadMozillaCABundle(ctx)
	if err != nil {
		return nil, fmt.Errorf("load Mozilla CA bundle: %w", err)
	}
	caBundle := append(append([]byte(nil), mozilla...), stagingChain...)
	return &Bundle{CACrt: caBundle, NodeExtraCA: stagingChain}, nil
}

func loadMozillaCABundle(ctx context.Context) ([]byte, error) {
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
	} {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 1024 {
			return raw, nil
		}
	}
	return fetchPEMFromURL(ctx, mozillaCABundleURL)
}

// fetchStagingCAChain walks AIA from the server leaf and returns PEM blocks for
// each issuing CA up to the staging root (excludes the server leaf itself).
func fetchStagingCAChain(leafPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(leafPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no leaf certificate in tls.crt")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}

	var chain []byte
	seen := map[string]struct{}{}
	urls := leaf.IssuingCertificateURL
	for step := 0; step < maxStagingCAChain && len(urls) > 0; step++ {
		url := urls[0]
		if _, ok := seen[url]; ok {
			break
		}
		seen[url] = struct{}{}

		issuerPEM, err := fetchPEMFromURL(context.Background(), url)
		if err != nil {
			break
		}
		chain = append(chain, issuerPEM...)

		issuer, err := parseFirstCertificate(issuerPEM)
		if err != nil {
			break
		}
		if issuer.Subject.String() == issuer.Issuer.String() {
			break
		}
		urls = issuer.IssuingCertificateURL
	}
	return chain, nil
}

func parseFirstCertificate(pemData []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func fetchPEMFromURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return normalizeCertPEM(raw)
}

// normalizeCertPEM accepts PEM or DER (Let's Encrypt AIA serves DER) and
// returns a PEM CERTIFICATE block.
func normalizeCertPEM(raw []byte) ([]byte, error) {
	if block, _ := pem.Decode(raw); block != nil && block.Type == "CERTIFICATE" {
		return pem.EncodeToMemory(block), nil
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
}

// EnsureStagingCASecret materializes gentian-staging-ca-tls in namespace when
// the kernel wildcard leaf secret exists. No-op when the leaf is absent
// (production clusters). Returns true when the target secret exists afterward.
func EnsureStagingCASecret(ctx context.Context, c client.Client, namespace, certManagerNS, leafSecretName string) (bool, error) {
	if namespace == "" {
		return false, fmt.Errorf("namespace is required")
	}
	if certManagerNS == "" {
		certManagerNS = DefaultCertManagerNS
	}
	if leafSecretName == "" {
		leafSecretName = DefaultLeafSecret
	}

	leaf := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: leafSecretName, Namespace: certManagerNS}, leaf); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read leaf secret %s/%s: %w", certManagerNS, leafSecretName, err)
	}
	leafPEM := leaf.Data["tls.crt"]
	if len(leafPEM) == 0 {
		return false, fmt.Errorf("leaf secret %s/%s has no tls.crt", certManagerNS, leafSecretName)
	}

	bundle, err := BuildBundle(ctx, leafPEM)
	if err != nil {
		return false, err
	}
	trustStore, err := BuildTrustStoreJKS(bundle.CACrt, TrustStorePassword)
	if err != nil {
		return false, fmt.Errorf("build truststore.jks: %w", err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gentian-os",
				"app.kubernetes.io/component":  "staging-ca-trust",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":       bundle.CACrt,
			NodeExtraCAKey: bundle.NodeExtraCA,
			TrustStoreKey:  trustStore,
		},
	}

	existing := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: SecretName, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		return true, c.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}
	existing.Type = desired.Type
	existing.Data = desired.Data
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	return true, c.Update(ctx, existing)
}
