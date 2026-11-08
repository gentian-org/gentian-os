/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The vault is verified against the CA file BAO_CACERT names, and only
// against it: a missing file or another CA fails the request instead of
// falling back to anything that does not verify.
func TestKVClientVerifiesTheVaultAgainstItsCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"data":{"k":"v"}}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	own := filepath.Join(dir, "ca.crt")
	writePEM(t, own, srv.Certificate().Raw)
	foreign := filepath.Join(dir, "other.crt")
	writePEM(t, foreign, otherCA(t))

	get := func(caFile string) error {
		t.Setenv("BAO_SKIP_VERIFY", "")
		t.Setenv("VAULT_SKIP_VERIFY", "")
		t.Setenv("BAO_CACERT", caFile)
		kv := secrets.NewKVClient(srv.URL, "r", "")
		kv.SetStaticToken("t")
		_, err := kv.Get(context.Background(), "p")
		return err
	}
	if err := get(own); err != nil {
		t.Errorf("the vault's own CA: %v", err)
	}
	if err := get(foreign); err == nil {
		t.Error("another CA verified the vault")
	}
	if err := get(filepath.Join(dir, "absent")); err == nil {
		t.Error("a CA file that is not there verified the vault")
	}
}

// otherCA is a self-signed CA certificate that signed nothing the server
// presents. (Every httptest server presents the same certificate.)
func otherCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writePEM(t *testing.T, path string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
