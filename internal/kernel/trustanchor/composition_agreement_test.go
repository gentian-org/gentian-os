/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package trustanchor

import (
	"os"
	"strings"
	"testing"
)

// TestCompositionMountsWhatWeWrite pins the two ends of a name that nothing else
// checks.
//
// This package replicates the anchor into each namespace under SecretName; the
// app Composition mounts a Secret by name. They are one fact in two files, and
// they disagreed: the Composition mounted the CLUSTER-scope secret named in
// certificates.trustAnchorSecret — which lives in cert-manager and is not in the
// app's namespace at all.
//
// The consequence is not a missing trust anchor. A volume naming a Secret that
// is not there stops the pod from starting, so every app on a self-signed or
// private-ca cluster would have failed to launch. Neither mode has been
// installed, which is the only reason it was not seen.
func TestCompositionMountsWhatWeWrite(t *testing.T) {
	raw, err := os.ReadFile("../../../crossplane/compositions/app-default.yaml")
	if err != nil {
		t.Skipf("composition not readable from here: %v", err)
	}
	comp := string(raw)

	if !strings.Contains(comp, `"`+SecretName+`"`) {
		t.Fatalf("app-default.yaml does not mount %q, which is the name this "+
			"package writes into every namespace", SecretName)
	}
	// The cluster-scope name must not be mounted: it is in cert-manager, and a
	// pod cannot mount another namespace's Secret.
	if strings.Contains(comp, `"secretName" "gentian-root-ca-tls"`) {
		t.Fatal("app-default.yaml mounts the cluster-scope anchor directly; " +
			"pods can only mount the per-namespace replica")
	}
}
