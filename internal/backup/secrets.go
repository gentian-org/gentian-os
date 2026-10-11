/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app's own secrets in a bundle.
//
// Every other artefact is produced by a Job: a container dumps a store, the
// next encrypts the file, the last uploads it. An app's secrets do not go
// that way. What a Job is handed is in its pod's specification or in a
// Secret beside it, and either would be a second copy of the values, in the
// cluster, readable by whoever reads pods or Secrets there. The operator
// reads the values from the vault, encrypts them itself, to the same
// recipients or passphrase as the rest of the bundle, and uploads the
// result: the values leave the operator's memory as ciphertext only.

// SecretsArtefact is where an app's secrets are in a bundle, below the
// bundle's prefix and before the suffix encryption adds.
func SecretsArtefact(app string) string { return "secrets/" + app + ".json" }

// ExtensionSecretName is how an extension's secret is named where only
// names are listed: in a status and in a message.
func ExtensionSecretName(extension, name string) string { return extension + "/" + name }

// SealAppSecrets encrypts an app's secrets as the bundle's other artefacts
// are encrypted: to the export's recipients, or under its passphrase.
// passphrase is read only in passphrase mode.
//
// The output is an ordinary age file, as the Jobs' are: whoever holds the
// key opens it with `age -d`.
func SealAppSecrets(e Encryption, passphrase string, secrets *bundle.AppSecrets) ([]byte, error) {
	if secrets.Empty() {
		return nil, errors.New("no secret to encrypt")
	}
	recipients, err := e.ageRecipients(passphrase)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, fmt.Errorf("encode the secrets: %w", err)
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipients...)
	if err != nil {
		return nil, fmt.Errorf("encrypt the secrets: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("encrypt the secrets: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("encrypt the secrets: %w", err)
	}
	return out.Bytes(), nil
}

// OpenAppSecrets decrypts an app's secrets artefact and reads it.
func OpenAppSecrets(cipher []byte, ids []age.Identity) (*bundle.AppSecrets, error) {
	plain, err := age.Decrypt(bytes.NewReader(cipher), ids...)
	if err != nil {
		return nil, fmt.Errorf("the secrets could not be opened with that key: %w", err)
	}
	var out bundle.AppSecrets
	if err := json.NewDecoder(plain).Decode(&out); err != nil {
		return nil, errors.New("the secrets do not parse")
	}
	return &out, nil
}

// ageRecipients are what the encryption encrypts to. There is no recipient
// that encrypts to nobody: an encryption that names none is an error, as it
// is for the Jobs (Validate).
func (e Encryption) ageRecipients(passphrase string) ([]age.Recipient, error) {
	switch e.Mode {
	case gentianov1alpha1.ExportEncryptionPassphrase:
		if passphrase == "" {
			return nil, errors.New("the passphrase is empty")
		}
		r, err := age.NewScryptRecipient(passphrase)
		if err != nil {
			return nil, err
		}
		return []age.Recipient{r}, nil
	case gentianov1alpha1.ExportEncryptionRecipient:
		if len(e.Recipients) == 0 {
			return nil, errors.New("no age recipients configured")
		}
		recipients, err := age.ParseRecipients(strings.NewReader(strings.Join(e.Recipients, "\n")))
		if err != nil {
			return nil, fmt.Errorf("the recipients do not parse: %w", err)
		}
		if len(recipients) != len(e.Recipients) {
			return nil, errors.New("not every recipient could be read")
		}
		return recipients, nil
	}
	return nil, fmt.Errorf("unknown encryption mode %q", e.Mode)
}
