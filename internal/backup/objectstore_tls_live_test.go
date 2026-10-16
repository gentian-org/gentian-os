/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// What the object store's lab needs besides the server: a CA standing in for
// the cluster's internal CA (gentian-ca), the certificates cert-manager
// issues from it in the store's namespace, the signing keys the vault holds,
// and the server's settings as the chart is given them.

// labCA is a throwaway CA, as cert-manager's CA issuer holds one.
type labCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newLabCA(t *testing.T, cn string) *labCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &labCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// issue is a certificate of this CA as cert-manager writes it into a Secret:
// tls.crt, tls.key and the CA's ca.crt.
func (ca *labCA) issue(t *testing.T, cn string, dnsNames ...string) map[string]string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"tls.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"tls.key": string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		"ca.crt":  ca.pem,
	}
}

// writeSecretDir writes a Secret's keys as the kubelet does into a volume,
// each key a file, and replaces them in place when called again on the same
// directory, as the kubelet does when the Secret changes.
func writeSecretDir(t *testing.T, dir string, data map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range data {
		tmp := filepath.Join(dir, "."+k+".new")
		if err := os.WriteFile(tmp, []byte(v), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, k)); err != nil {
			t.Fatal(err)
		}
	}
}

// chartServerSettings are the environment and the certificate's mount path
// the server is given by the platform's values for it
// (kernel/services/infra-seaweedfs/manifests/templates/configmap.yaml, the
// base values), read from that file so the lab runs what the cluster runs.
type chartServerSettings struct {
	env       map[string]string
	secretEnv map[string]corev1.SecretKeySelector
	tlsDir    string
	tlsSecret string
}

var templateExpr = regexp.MustCompile(`\{\{[^}]*\}\}`)

func readChartServerSettings(t *testing.T) chartServerSettings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "kernel", "services", "infra-seaweedfs", "manifests", "templates", "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(templateExpr.ReplaceAllString(string(raw), "x"), "\n---\n") {
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil || cm.Name != "seaweedfs-base-values" {
			continue
		}
		var values struct {
			AllInOne struct {
				ExtraEnvironmentVars       map[string]string              `json:"extraEnvironmentVars"`
				SecretExtraEnvironmentVars map[string]corev1.EnvVarSource `json:"secretExtraEnvironmentVars"`
				ExtraVolumes               string                         `json:"extraVolumes"`
				ExtraVolumeMounts          string                         `json:"extraVolumeMounts"`
			} `json:"allInOne"`
		}
		if err := yaml.Unmarshal([]byte(cm.Data["values.yaml"]), &values); err != nil {
			t.Fatalf("the base values do not parse: %v", err)
		}
		s := chartServerSettings{env: values.AllInOne.ExtraEnvironmentVars, secretEnv: map[string]corev1.SecretKeySelector{}}
		for name, src := range values.AllInOne.SecretExtraEnvironmentVars {
			if src.SecretKeyRef == nil {
				t.Fatalf("%s is not read from a Secret", name)
			}
			s.secretEnv[name] = *src.SecretKeyRef
		}
		var volumes []corev1.Volume
		var mounts []corev1.VolumeMount
		if err := yaml.Unmarshal([]byte(values.AllInOne.ExtraVolumes), &volumes); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(values.AllInOne.ExtraVolumeMounts), &mounts); err != nil {
			t.Fatal(err)
		}
		if len(volumes) != 1 || volumes[0].Secret == nil || len(mounts) != 1 || mounts[0].Name != volumes[0].Name || mounts[0].SubPath != "" {
			t.Fatalf("the base values mount %v at %v, want one Secret, whole", volumes, mounts)
		}
		s.tlsSecret, s.tlsDir = volumes[0].Secret.SecretName, mounts[0].MountPath
		return s
	}
	t.Fatal("configmap.yaml has no seaweedfs-base-values")
	return chartServerSettings{}
}

// labJWT is a token for one file id, as the master signs it for a write,
// signed with the key given.
func labJWT(key, fid string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"fid": fid, "exp": time.Now().Add(time.Minute).Unix()})
	unsigned := header + "." + enc.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil))
}
