/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"reflect"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// An app whose chart the operator installs carries its release's labels, not
// the app label: its own policies -- the way to what it declared of the
// platform, between its own pods, and the egress somebody granted it -- are
// for those pods, or they hold for no pod and the app is cut off.
func TestAnAppsOwnPoliciesSelectThePodsItHas(t *testing.T) {
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Database: &gentianov1alpha1.DatabaseRequirement{}}}
	release := map[string]string{"app.kubernetes.io/instance": "tenant-acme-wiki"}
	in := BuildInput{
		TenantName:   "acme",
		Namespace:    "tenant-acme",
		Apps:         []gentianov1alpha1.TenantApp{{Profile: "wiki"}, {Profile: "shop"}},
		Profiles:     map[string]*gentianov1alpha1.ComponentProfile{"wiki": profile, "shop": profile},
		PodSelectors: map[string]map[string]string{"wiki": release},
	}
	seen := 0
	for _, np := range BuildDesired(in) {
		switch np.Name {
		case kernelPolicyName("wiki"), appInternalPolicyName("wiki"):
			seen++
			if !reflect.DeepEqual(np.Spec.PodSelector.MatchLabels, release) {
				t.Errorf("%s selects %v, want the release's pods", np.Name, np.Spec.PodSelector.MatchLabels)
			}
			for _, rule := range np.Spec.Ingress {
				for _, from := range rule.From {
					if from.PodSelector != nil && !reflect.DeepEqual(from.PodSelector.MatchLabels, release) {
						t.Errorf("%s admits %v", np.Name, from.PodSelector.MatchLabels)
					}
				}
			}
		case kernelPolicyName("shop"):
			seen++
			if np.Spec.PodSelector.MatchLabels[meta.AppLabel] != "shop" {
				t.Errorf("an app the Composition delivers is selected by %v", np.Spec.PodSelector.MatchLabels)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("saw %d of the three policies", seen)
	}
}
