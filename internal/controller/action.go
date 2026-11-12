/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// actionInstalled says what an installed action is, in the words its status
// carries: the image a run of it starts, at the one build it may run. Nothing
// is deployed for an action, and a status that said "release deployed" would
// claim a workload that does not exist.
func actionInstalled(profile *gentianov1alpha1.ComponentProfile) string {
	if len(profile.Spec.Package.Images) != 1 {
		// Admission refuses such a profile; one written before the rule
		// existed is said as it is.
		return "action installed; its package does not name exactly one image, so nothing can run it"
	}
	image := profile.Spec.Package.Images[0]
	return fmt.Sprintf("action %s@%s installed; nothing is deployed for an action",
		image.Repository, image.Digest)
}
