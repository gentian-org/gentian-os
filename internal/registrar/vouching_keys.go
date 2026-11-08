/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import "github.com/gentian-org/gentian-os/internal/componentkeys"

// VouchingKey is one component that vouches for people, as the operator
// lists it for the registrar (the ConfigMap registrar-vouching-keys,
// internal/controller/vouching_keys.go). Its Component is the profile it
// runs, which is what its entry in the realm is named after.
type VouchingKey = componentkeys.Key

// VouchingKeys is the list the registrar currently holds, followed as
// VouchingKeysList. The zero value, and nil, hold none.
type VouchingKeys = componentkeys.List[VouchingKey]

// VouchingKeysList names the list in the log and in a refusal.
const VouchingKeysList = "vouching keys"

// ParseVouchingKeys reads the operator's list, refused whole by
// componentkeys.Check.
func ParseVouchingKeys(raw []byte) ([]VouchingKey, error) {
	return componentkeys.Parse[VouchingKey](raw, VouchingKeysList)
}
