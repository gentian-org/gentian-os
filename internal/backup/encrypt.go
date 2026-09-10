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

package backup

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// EncryptedSuffix is appended to every artefact in a bundle. Bundles are always
// encrypted, so the suffix is not a variant — it is what the files are called.
const EncryptedSuffix = ".age"

// PassphraseEnvVar carries the passphrase into the encryption container in
// mode: passphrase.
const PassphraseEnvVar = "GENTIAN_BUNDLE_PASSPHRASE"

// Encryption is the resolved protection for one export.
type Encryption struct {
	Mode gentianov1alpha1.ExportEncryptionMode

	// Recipients are age public keys, used in recipient mode.
	Recipients []string

	// PassphraseSecret names a Secret in the Job's own namespace holding the
	// passphrase, used in passphrase mode. The controller places it there and
	// removes it when the export finishes.
	PassphraseSecret string
	PassphraseKey    string
}

// Validate reports why an encryption setting cannot be used.
//
// There is no unencrypted path, and this is where that is enforced. An export
// that cannot be protected does not run: a bundle is every byte a tenant has,
// it outlives the cluster that made it, and it ends up in places nobody is
// watching. Failing the export is recoverable; a plaintext copy of a tenant is
// not.
func (e Encryption) Validate() error {
	switch e.Mode {
	case gentianov1alpha1.ExportEncryptionRecipient:
		if len(e.Recipients) == 0 {
			return fmt.Errorf(
				"no age recipients configured: set backupRecipients on the operator " +
					"(BACKUP_AGE_RECIPIENTS), or request mode: passphrase")
		}
		for _, r := range e.Recipients {
			if !strings.HasPrefix(r, "age1") {
				return fmt.Errorf("recipient %q is not an age public key", r)
			}
		}
	case gentianov1alpha1.ExportEncryptionPassphrase:
		if e.PassphraseSecret == "" {
			return fmt.Errorf("mode: passphrase requires spec.encryption.passphraseSecretRef")
		}
	default:
		return fmt.Errorf("unknown encryption mode %q", e.Mode)
	}
	return nil
}

// PlatformReadable reports whether a platform-held identity can open the
// bundle. Only recipient mode against the cluster's own recipients is, and the
// controller decides that by comparing against its configured set.
func (e Encryption) PlatformReadable(clusterRecipients []string) bool {
	if e.Mode != gentianov1alpha1.ExportEncryptionRecipient {
		return false
	}
	for _, configured := range clusterRecipients {
		for _, used := range e.Recipients {
			if configured == used {
				return true
			}
		}
	}
	return false
}

// encryptContainer builds the step that encrypts one staged artefact in place.
//
// It runs between producing the artefact and uploading it, and deletes the
// plaintext before exiting, so an upload can only ever find the encrypted file.
// Ordering it after the upload, or leaving the plaintext for the uploader to
// skip, would both mean a crash at the wrong moment leaves a tenant's data
// readable in the bundle.
func encryptContainer(e Encryption, localFile string) corev1.Container {
	plain := workDir + "/" + localFile
	cipher := plain + EncryptedSuffix

	var encrypt string
	switch e.Mode {
	case gentianov1alpha1.ExportEncryptionPassphrase:
		// age reads a passphrase from the terminal and refuses a pipe, so the
		// only way to use it unattended is to give it one. script(1) provides
		// the pty; age still does the cryptography, which is what keeps the
		// output an ordinary age file the recipient can open with `age -d`.
		encrypt = fmt.Sprintf(`if [ -z "${%[1]s:-}" ]; then
  echo "ERROR: passphrase is empty" >&2; exit 1
fi
printf '%%s\n%%s\n' "${%[1]s}" "${%[1]s}" \
  | script -qec "age -p -o '%[2]s' '%[3]s'" /dev/null >/dev/null`,
			PassphraseEnvVar, cipher, plain)
	default:
		var recipients strings.Builder
		for _, r := range e.Recipients {
			fmt.Fprintf(&recipients, " -r %s", shellSingleQuote(r))
		}
		encrypt = fmt.Sprintf("age%s -o '%s' '%s'", recipients.String(), cipher, plain)
	}

	script := fmt.Sprintf(`set -eu
%s
%s

# The plaintext must not outlive this step: the uploader runs next and would
# otherwise have a readable copy to find.
rm -f '%s'
[ -s '%s' ] || { echo "ERROR: encryption produced no output" >&2; exit 1; }
echo "encrypted %s"`, encryptBootstrap(e), encrypt, plain, cipher, localFile)

	container := corev1.Container{
		Name:         "encrypt",
		Image:        kernel.KeycloakProvisionerImage(),
		Command:      []string{"/bin/sh", "-c"},
		Args:         []string{script},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	if e.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		container.Env = []corev1.EnvVar{
			meta.SecretEnv(PassphraseEnvVar, e.PassphraseSecret, e.PassphraseKey),
		}
	}
	return container
}

// encryptBootstrap installs age, and script(1) when a passphrase needs a pty.
//
// Verified rather than assumed: a missing tool must fail here, loudly, and not
// later as an upload that quietly carried plaintext.
func encryptBootstrap(e Encryption) string {
	packages := "age"
	required := "age"
	if e.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		// script(1) lives in util-linux-misc on recent Alpine and in util-linux
		// on older ones; ask for whichever is present.
		packages = "age util-linux-misc"
		required = "age script"
	}
	return fmt.Sprintf(`apk add --no-cache --quiet %[1]s >/dev/null 2>&1 \
  || apk add --no-cache --quiet age util-linux >/dev/null 2>&1 \
  || { echo "ERROR: could not install %[1]s" >&2; exit 1; }
for tool in %[2]s; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "ERROR: ${tool} unavailable" >&2; exit 1; }
done`, packages, required)
}

// BundleInfo is the one file in a bundle that is not encrypted.
//
// It says what the bundle is and how to open it, and nothing about what is
// inside. Without it a recipient facing a directory of .age files has to guess
// which identity applies; with it, the manifest — which carries the tenant's
// spec and app inventory — can stay encrypted like everything else.
type BundleInfo struct {
	SchemaVersion int      `json:"schemaVersion"`
	Tenant        string   `json:"tenant"`
	Export        string   `json:"export"`
	CreatedAt     string   `json:"createdAt"`
	Encryption    string   `json:"encryption"`
	Recipients    []string `json:"recipients,omitempty"`
	// HowToDecrypt is a literal command, because the person reading this is
	// having a bad day and should not have to look anything up.
	HowToDecrypt string `json:"howToDecrypt"`
}

// NewBundleInfo builds the unencrypted header for a bundle.
func NewBundleInfo(tenant, export, createdAt string, e Encryption) *BundleInfo {
	info := &BundleInfo{
		SchemaVersion: ManifestSchemaVersion,
		Tenant:        tenant,
		Export:        export,
		CreatedAt:     createdAt,
		Encryption:    string(e.Mode),
	}
	if e.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		info.HowToDecrypt = "age -d manifest.json.age > manifest.json  # prompts for the passphrase"
	} else {
		info.Recipients = e.Recipients
		info.HowToDecrypt = "age -d -i <identity-file> manifest.json.age > manifest.json"
	}
	return info
}
