/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import "github.com/gentian-org/gentian-os/internal/componentkeys"

// CommissionKey is one component that may commission partners, as the
// operator lists it for the director (the ConfigMap director-commission-keys,
// internal/controller/partner_commissioners.go), with the tenants whose realm
// it may add an outside realm to.
type CommissionKey = componentkeys.Commission

// CommissionKeys is the list the director currently holds, followed as
// CommissionKeysList. The zero value, and nil, hold none.
type CommissionKeys = componentkeys.List[CommissionKey]

// CommissionKeysList names the list in the log and in a refusal.
const CommissionKeysList = "commission keys"

// ParseCommissionKeys reads the operator's list, refused whole as the
// registrar refuses its own (componentkeys.Check).
func ParseCommissionKeys(raw []byte) ([]CommissionKey, error) {
	return componentkeys.Parse[CommissionKey](raw, CommissionKeysList)
}
