/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func TestTSIGSecretShape(t *testing.T) {
	v := NewEndpointValidator()
	check := func(secret string) error {
		return v.Validate(context.Background(), "tsig-secret", "", map[string]string{"tsig-secret": secret})
	}

	// What pdnsutil generate-tsig-key prints for hmac-sha512: 64 bytes.
	if err := check(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 64)))); err != nil {
		t.Fatalf("a 64-byte key was refused: %v", err)
	}
	for name, secret := range map[string]string{
		"empty":        "",
		"a key name":   "gentian-key.",
		"cut short":    base64.StdEncoding.EncodeToString([]byte("short")),
		"lost padding": strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 17))), "="),
		"with a space": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) + " ",
	} {
		if err := check(secret); err == nil {
			t.Errorf("%s: accepted %q", name, secret)
		}
	}
}
