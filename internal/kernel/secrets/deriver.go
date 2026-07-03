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

package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Deriver turns the platform master password into per-credential values using
// HKDF-SHA256 (RFC 5869) — a state-of-the-art KDF for diversifying a single
// high-entropy master into many sub-keys.
//
// The derivation is fully deterministic: same (master, salt, info, length) →
// same bytes. This means an app uninstalled and reinstalled gets the same
// credentials back without needing any persistent state outside OpenBao.
//
// Salts are canonical KV paths (see CategoryPath, InternalPath, KernelPath).
// Tenant-scoped paths include the tenant name, kernel-shared paths do not —
// so a single shared master is collision-free across tenants.
type Deriver struct {
	master []byte
}

// NewDeriver constructs a Deriver from the master password. An empty master
// is allowed (the operator falls back to a random generator in that case),
// but callers should treat NewDeriver("") as a misconfiguration in
// production.
func NewDeriver(master string) *Deriver {
	return &Deriver{master: []byte(master)}
}

// HasMaster reports whether the deriver has a non-empty master password.
func (d *Deriver) HasMaster() bool { return len(d.master) > 0 }

// Derive returns n hex characters derived from (master, salt, info).
//
//   - salt: a globally-unique stable identifier for this credential
//     (the canonical KV path).
//   - info: an optional sub-field tag (e.g. "password", "client-secret")
//     used when one path stores multiple derived fields.
//   - n: number of hex characters to return (40 ≈ 160 bits is the default
//     across the platform; values like 20 are used for S3 access keys).
//
// If n ≤ 0, defaults to 40. n is clamped to ≤ 64 (a single SHA-256 block).
func (d *Deriver) Derive(salt, info string, n int) string {
	if n <= 0 {
		n = 40
	}
	if n > 64 {
		n = 64
	}
	r := hkdf.New(sha256.New, d.master, []byte(salt), []byte(info))
	out := make([]byte, (n+1)/2)
	if _, err := io.ReadFull(r, out); err != nil {
		// HKDF over SHA-256 cannot fail for outputs ≤ 255*32 bytes.
		panic("secrets.Derive: hkdf read failed: " + err.Error())
	}
	return hex.EncodeToString(out)[:n]
}
