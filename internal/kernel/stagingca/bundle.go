// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

// Package stagingca builds the gentian-staging-ca-tls trust bundle for ACME
// staging clusters. openDesk charts mount this secret so in-cluster OIDC
// clients (Synapse, Jitsi adapter) trust https://id.<kernel-domain>.
package stagingca

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
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

	DefaultCertManagerNS = "cert-manager"
	DefaultLeafSecret    = "wildcard-kernel-tls"
)

// BuildBundle returns a PEM CA bundle from a leaf certificate plus its issuing
// intermediate(s) fetched via AIA. Matches scripts/create-staging-ca-secret.sh.
func BuildBundle(leafPEM []byte) ([]byte, error) {
	if len(leafPEM) == 0 {
		return nil, fmt.Errorf("empty leaf certificate")
	}
	bundle := append([]byte(nil), leafPEM...)
	rest := leafPEM
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse leaf certificate: %w", err)
		}
		for _, url := range cert.IssuingCertificateURL {
			intermediate, err := fetchPEM(url)
			if err != nil {
				continue
			}
			if len(intermediate) > 0 {
				bundle = append(bundle, '\n')
				bundle = append(bundle, intermediate...)
			}
		}
		break
	}
	return bundle, nil
}

func fetchPEM(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
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

	bundle, err := BuildBundle(leafPEM)
	if err != nil {
		return false, err
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
		Data: map[string][]byte{"ca.crt": bundle},
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
