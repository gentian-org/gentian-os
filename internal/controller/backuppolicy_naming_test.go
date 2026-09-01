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
