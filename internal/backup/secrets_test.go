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
	"reflect"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app's secrets are encrypted as the rest of its bundle is: to every
// recipient of the export, or under its passphrase, and to nothing where the
// export names nobody. There is no way to get them out unencrypted.
func TestAppSecretsAreSealedToTheBundlesOwnKey(t *testing.T) {
	doc := &bundle.AppSecrets{App: "wiki", Secrets: map[string]string{"encryption_key": "0123456789abcdef"},
		Extensions: map[string]map[string]string{"search": {"index_key": "fedcba9876543210"}}}
	one, _ := age.GenerateX25519Identity()
	two, _ := age.GenerateX25519Identity()
	stranger, _ := age.GenerateX25519Identity()

	sealed, err := SealAppSecrets(Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient,
		Recipients: []string{one.Recipient().String(), two.Recipient().String()}}, "ignored", doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte("age-encryption.org/v1")) || bytes.Contains(sealed, []byte("0123456789abcdef")) || bytes.Contains(sealed, []byte("encryption_key")) {
		t.Fatalf("not ciphertext: %.60q", sealed)
	}
	for name, id := range map[string]age.Identity{"the first recipient": one, "the second recipient": two} {
		got, err := OpenAppSecrets(sealed, []age.Identity{id})
		if err != nil || !reflect.DeepEqual(got, doc) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := OpenAppSecrets(sealed, []age.Identity{stranger}); err == nil {
		t.Error("opened by a key the bundle was not encrypted to")
	}

	withPassphrase, err := SealAppSecrets(Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase}, "correct horse", doc)
	if err != nil {
		t.Fatal(err)
	}
	right, _ := age.NewScryptIdentity("correct horse")
	wrong, _ := age.NewScryptIdentity("battery staple")
	if got, err := OpenAppSecrets(withPassphrase, []age.Identity{right}); err != nil || !reflect.DeepEqual(got, doc) {
		t.Errorf("under the passphrase: %v", err)
	}
	if _, err := OpenAppSecrets(withPassphrase, []age.Identity{wrong}); err == nil {
		t.Error("opened under another passphrase")
	}
	if _, err := OpenAppSecrets(withPassphrase, []age.Identity{one}); err == nil {
		t.Error("a passphrase's file opened by a recipient's key")
	}

	for name, e := range map[string]Encryption{
		"no recipient":         {Mode: gentianov1alpha1.ExportEncryptionRecipient},
		"not a key":            {Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{"age1notakey"}},
		"one of two not a key": {Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{one.Recipient().String(), "nonsense"}},
		"no passphrase":        {Mode: gentianov1alpha1.ExportEncryptionPassphrase},
		"no mode":              {},
	} {
		if out, err := SealAppSecrets(e, "", doc); err == nil {
			t.Errorf("%s: sealed %d bytes", name, len(out))
		}
	}
	if _, err := SealAppSecrets(Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase}, "p", &bundle.AppSecrets{App: "wiki"}); err == nil {
		t.Error("an artefact with no secret in it was made")
	}
	// A failure to read says nothing of what was read.
	var buf bytes.Buffer
	w, _ := age.Encrypt(&buf, one.Recipient())
	_, _ = w.Write([]byte(`{"app":"wiki","secrets":{"encryption_key":"0123456789abcdef"`))
	_ = w.Close()
	if _, err := OpenAppSecrets(buf.Bytes(), []age.Identity{one}); err == nil || strings.Contains(err.Error(), "0123456789abcdef") {
		t.Errorf("a document that does not parse: %v", err)
	}
}

// The file's format is the API's: names and values, and no field that could
// say where a value goes.
func TestAnAppsSecretsDocumentHoldsNamesAndValuesAndNoPath(t *testing.T) {
	raw, err := json.Marshal(&bundle.AppSecrets{App: "wiki", Secrets: map[string]string{"a": "1"},
		Extensions: map[string]map[string]string{"search": {"b": "2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"app":"wiki","secrets":{"a":"1"},"extensions":{"search":{"b":"2"}}}` {
		t.Errorf("the document is %s", raw)
	}
	if SecretsArtefact("wiki") != "secrets/wiki.json" || !slices.Contains(bundle.AppArtefacts, bundle.ArtefactSecrets) {
		t.Errorf("the artefact is %q", SecretsArtefact("wiki"))
	}
	if !(&bundle.AppSecrets{App: "wiki", Extensions: map[string]map[string]string{"search": {}}}).Empty() || (&bundle.AppSecrets{Secrets: map[string]string{"a": "1"}}).Empty() {
		t.Error("Empty is wrong")
	}
}
