/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The Policy refuses a pinned repository at any other build, in every kind
// of container, enforced and failing closed.
func TestImagePinsHoldEveryContainerToTheBuild(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "vault", Namespace: "tenant-platform"}}
	policy := imagePinsPolicy(comp, []gentianov1alpha1.PinnedImage{{Repository: "registry.example/acme/vault", Digest: digest}})

	if policy.GetName() != "image-pins-vault" || policy.GetNamespace() != "tenant-platform" {
		t.Fatalf("policy = %s/%s", policy.GetNamespace(), policy.GetName())
	}
	spec := policy.Object["spec"].(map[string]interface{})
	if spec["validationFailureAction"] != "Enforce" || spec["failurePolicy"] != "Fail" {
		t.Fatalf("spec = %+v", spec)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"registry.example/acme/vault:*"`,
		`"registry.example/acme/vault@*"`,
		`"registry.example/acme/vault@` + digest + `"`,
		"spec.containers",
		"spec.initContainers",
		"spec.ephemeralContainers",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the policy does not say %s: %s", want, raw)
		}
	}
}
