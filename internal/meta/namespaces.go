/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package meta

import "github.com/gentian-org/gentian-os/internal/layout"

const (
	RoutingModeGateway = "gateway"
)

// OperatorNamespace is where the operator runs: the control function of the
// layout, read from the environment the chart sets.
var OperatorNamespace = layout.Namespace(layout.Control)
