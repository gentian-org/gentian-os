/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A BackupPolicy's name is load-bearing: every reader fetches one by name — the
// cluster's as "default", a tenant's as the tenant's own name — so a policy
// named anything else is read by nothing while still reporting itself Accepted
// and publishing an effective destination. Bundles then go to the platform's
// own storage as though the policy had never been written.
//
// The rules are CEL on the CRD, so a real API server is the only honest place
// to test them: a fake client applies no schema validation and would pass
// whatever this file asserted.
func TestBackupPolicyNameMustMatchItsScope(t *testing.T) {
	ctx := context.Background()

	policy := func(name, scope, tenant string) *gentianov1alpha1.BackupPolicy {
		return &gentianov1alpha1.BackupPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gentianov1alpha1.BackupPolicySpec{
				Scope:    scope,
				Tenant:   tenant,
				Schedule: "0 3 * * *",
			},
		}
	}

	cases := []struct {
		name     string
		obj      *gentianov1alpha1.BackupPolicy
		admitted bool
	}{
		{"tenant policy named after its tenant", policy("acme", "tenant", "acme"), true},
		{"tenant policy named anything else", policy("nightly-exoscale", "tenant", "acme"), false},
		{"cluster policy named default", policy("default", "cluster", ""), true},
		{"cluster policy named anything else", policy("cluster-wide", "cluster", ""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testClient.Create(ctx, tc.obj)
			if tc.admitted {
				if err != nil {
					t.Fatalf("rejected a correctly named policy: %v", err)
				}
				t.Cleanup(func() { _ = testClient.Delete(ctx, tc.obj) })
				return
			}
			if err == nil {
				_ = testClient.Delete(ctx, tc.obj)
				t.Fatal("admitted a misnamed policy; nothing would ever read it, " +
					"and it would report Accepted while bundles went elsewhere")
			}
			if !strings.Contains(err.Error(), "the name the operator reads it by") {
				t.Errorf("rejected, but not by the naming rule: %v", err)
			}
		})
	}
}
